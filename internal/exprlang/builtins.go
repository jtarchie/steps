package exprlang

import (
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
