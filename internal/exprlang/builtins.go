package exprlang

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// envFunc builds env(name) / env(name, default).
//
// It resolves ONLY names the resource type listed in env:, with no baseline
// allowlist. A shell command inherits PATH, HOME and TMPDIR because it goes
// on to run real tools that need them; an expression runs in-process and
// runs nothing, so there is no reason for it to see any variable a pipeline
// did not name. The allowlist is therefore the whole list, and reading a
// credential stays a declaration you can grep for.
//
// A name that is allowed but unset is an error rather than an empty string:
// an unset token silently produces an unauthenticated request, which fails
// later, further away, and as something else. Pass a default to opt into
// that: env("TOKEN", "").
func envFunc(allow []string) func(...any) (any, error) {
	return func(params ...any) (any, error) {
		name, fallback, hasFallback, err := nameAndFallback("env", params)
		if err != nil {
			return nil, err
		}

		if !slices.Contains(allow, name) {
			if len(allow) == 0 {
				return nil, fmt.Errorf("env(%q): this resource type declares no env:, so no variable is readable", name)
			}

			return nil, fmt.Errorf("env(%q): not in this resource type's env: [%s]", name, strings.Join(allow, " "))
		}

		value, ok := os.LookupEnv(name)
		if !ok {
			if hasFallback {
				return fallback, nil
			}

			return nil, fmt.Errorf("env(%q): %w", name, errUnsetEnv)
		}

		return value, nil
	}
}

// fileFunc builds file(path) / file(path, default), reading from the put's
// read view — the same directory a shell out: gets as its cwd, holding the
// artifacts the put declared as inputs.
//
// Paths are relative to that directory and may not escape it. filepath.IsLocal
// is the whole guard: it rejects absolute paths, "..", and the roundabout
// spellings of "..", which a hand-rolled prefix check gets wrong.
func fileFunc(dir string) func(...any) (any, error) {
	return func(params ...any) (any, error) {
		path, fallback, hasFallback, err := nameAndFallback("file", params)
		if err != nil {
			return nil, err
		}

		if !filepath.IsLocal(path) {
			return nil, fmt.Errorf("file(%q): must be a relative path inside the put's inputs", path)
		}

		data, err := os.ReadFile(filepath.Join(dir, path)) //nolint:gosec // IsLocal above confines the path to dir
		if err != nil {
			if os.IsNotExist(err) && hasFallback {
				return fallback, nil
			}

			return nil, fmt.Errorf("file(%q): %w", path, err)
		}

		return string(data), nil
	}
}

// versionFunc builds version(name) / version(): the version a get called name
// fetched in this build, or, with no name, the one fetched input's version
// (nil when there is none, so a type can fall back to reading files).
//
// Only the put's inputs are visible — the same boundary as file(), so a put
// cannot read what a get fetched for an artifact it did not declare.
func versionFunc(inputs []string, versions map[string]map[string]any) func(...any) (any, error) {
	return func(params ...any) (any, error) {
		if len(params) == 0 {
			return onlyFetchedVersion(inputs, versions)
		}

		if len(params) > 1 {
			return nil, fmt.Errorf("version() takes a get's name or nothing, got %d arguments", len(params))
		}

		name, ok := params[0].(string)
		if !ok {
			return nil, fmt.Errorf("version(): first argument is %T, want a string", params[0])
		}

		if !slices.Contains(inputs, name) {
			if len(inputs) == 0 {
				return nil, fmt.Errorf("version(%q): this put has no inputs, so it can read no version", name)
			}

			return nil, fmt.Errorf("version(%q): not an input of this put; its inputs are [%s]", name, strings.Join(inputs, " "))
		}

		version, fetched := versions[name]
		if !fetched {
			return nil, fmt.Errorf("version(%q): input %q was not fetched by a get in this build, so it has no version", name, name)
		}

		return copyVersion(version)
	}
}

func onlyFetchedVersion(inputs []string, versions map[string]map[string]any) (any, error) {
	var fetched []string

	for _, name := range inputs {
		if _, ok := versions[name]; ok {
			fetched = append(fetched, name)
		}
	}

	switch len(fetched) {
	case 0:
		return nil, nil //nolint:nilnil // no fetched input is the answer that lets a type fall back to files
	case 1:
		return copyVersion(versions[fetched[0]])
	default:
		return nil, fmt.Errorf("version(): this put has %d fetched inputs [%s]; name one, e.g. version(%q)",
			len(fetched), strings.Join(fetched, " "), fetched[0])
	}
}

// copyVersion hands the expression its own copy. The map is shared with the
// build's input set and resolution cache, and normalizing it in place would
// turn json.Number("1.50") into 1.5 — a different encoding, so a different
// stored identity. Numbers are normalized on the copy so they compare the way
// http()'s do.
func copyVersion(version map[string]any) (any, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return nil, fmt.Errorf("version(): %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()

	var copied map[string]any

	err = decoder.Decode(&copied)
	if err != nil {
		return nil, fmt.Errorf("version(): %w", err)
	}

	return normalizeNumbers(orEmpty(copied)), nil
}

// failFunc builds fail(message), the only way an expression can refuse.
//
// It exists because "the request succeeded and the API said no" is the normal
// shape of a JSON API: Slack answers 200 with {"ok": false, "error":
// "not_in_channel"}, and so do plenty of others. http() deliberately treats a
// status as data, so without this an out: that failed to post would return a
// version-less nil — indistinguishable from a put that legitimately published
// nothing, and the step would go green having done nothing at all.
//
// Paired with the ternary (which short-circuits), this reads as a guard:
//
//	posted.ok ? {channel: posted.channel} : fail("slack: " + posted.error)
//
// Deliberately not a general try/catch's other half — there is no catching
// anything here. This is a way to say no, which a language with no statements
// otherwise has no way to express.
func failFunc() func(...any) (any, error) {
	return func(params ...any) (any, error) {
		if len(params) != 1 {
			return nil, fmt.Errorf("fail() takes one message, got %d arguments", len(params))
		}

		return nil, errors.New(scalarString(params[0]))
	}
}

// nameAndFallback reads the (name) or (name, default) argument shape both
// env() and file() take. expr checks arity only for functions declared with
// types, and declaring two overloads is more machinery than the check below.
func nameAndFallback(fn string, params []any) (name string, fallback any, hasFallback bool, err error) {
	if len(params) == 0 || len(params) > 2 {
		return "", nil, false, fmt.Errorf("%s() takes a name and an optional default, got %d arguments", fn, len(params))
	}

	name, ok := params[0].(string)
	if !ok {
		return "", nil, false, fmt.Errorf("%s(): first argument is %T, want a string", fn, params[0])
	}

	if len(params) == 2 {
		return name, params[1], true, nil
	}

	return name, nil, false, nil
}
