// Package exprlang evaluates the expression form of a resource type's
// check/in/out — the JSON-over-HTTP alternative to writing those three as
// shell commands.
//
// It is a leaf: it knows nothing of pipelines, resources, or the store.
// Everything context-specific (which env names are allowed, which directory
// file() may read, the source/version/params in scope) arrives in an Input,
// so the only thing this package decides is what the language can do.
//
// The language is expr-lang/expr, chosen for what it CANNOT do. It has no
// statements, no loops, no assignment, and no I/O except through the
// functions handed to it here — so a resource type written this way cannot
// run a shell command, cannot outlive its call, and cannot be made to by any
// amount of cleverness in a pipeline file. Adding a scripting engine with
// those capabilities would have meant owning a sandbox; this way there is
// nothing to sandbox.
//
// What the expression form buys over shell, concretely: no dependency on
// curl/jq being installed, no quoting hazard (values are values, never text
// spliced into a command), errors that propagate instead of being swallowed
// by a pipeline's exit status, and HTTP that can be concurrent and
// rate-limit-aware because steps owns the fan-out (see http.go).
package exprlang

import (
	"context"
	"errors"
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
)

// Slot names one of the three lifecycle stages an expression can implement.
// The stages differ in what they see and what they may do, so the slot is a
// parameter to everything here rather than a caller convention.
type Slot string

// The three lifecycle stages, spelled as they are in YAML so an error
// message reads as the key the author wrote.
const (
	SlotCheck Slot = "check"
	SlotIn    Slot = "in"
	SlotOut   Slot = "out"
)

// Input is everything an expression evaluates against.
type Input struct {
	// Source is the resource's source: block. Always in scope.
	Source map[string]any
	// Version is the version being fetched (in), or the check cursor — the
	// last version this pipeline recorded (check). Never in scope for out,
	// which produces a version rather than consuming one.
	Version map[string]any
	// Params are the get's or put's params:. In scope for in and out.
	Params map[string]any
	// EnvAllow is the resource type's env: list, and the complete set of
	// names env() will resolve. See envFunc for why there is no baseline.
	EnvAllow []string
	// Dir is the put's read view, the only directory file() may read from.
	// Out slot only.
	Dir string
}

// Compile parses and type-checks one slot's expression without running it, so
// a syntax error is reported by `steps validate` and by preflight rather than
// halfway through a watch loop.
//
// It cannot be reported at LOAD time: internal/config depends on nothing
// internal and imports no third-party code but the YAML parser (enforced by
// depguard), which is what keeps the config types a leaf everything else can
// agree on. A syntax error is therefore a validate-time fact, not a
// parse-time one — see resource.CompileExprPrograms for where that is wired.
func Compile(slot Slot, src string) error {
	_, _, err := compileProgram(context.Background(), slot, src, Input{})

	return err
}

// compileProgram builds the program and the environment it runs against.
//
// Compiling per call rather than caching is deliberate: the builtins close
// over this call's context, directory and env allowlist, so a cached program
// would carry another call's permissions. A compile is microseconds against
// an HTTP round trip, which is what every expression here exists to make.
func compileProgram(ctx context.Context, slot Slot, src string, in Input) (*vm.Program, map[string]any, error) {
	env := slotEnv(slot, in)

	funcs := slotFuncs(ctx, slot, in)
	bans := slotBuiltinBans(slot)

	options := make([]expr.Option, 0, 1+len(funcs)+len(bans))
	options = append(options, expr.Env(env))
	options = append(options, funcs...)
	options = append(options, bans...)

	program, err := expr.Compile(src, options...)
	if err != nil {
		return nil, nil, fmt.Errorf("expr.%s: %w", slot, err)
	}

	return program, env, nil
}

// slotEnv builds the variables in scope for a slot. Every map is non-nil,
// including on the Compile path where there are no values yet: the checker
// reads the env for its TYPES, and a nil map would make `source.channels` a
// compile error about nil rather than the legal expression it is.
//
// Version is present-but-empty on the first-ever check, which is what makes
// `version.ts ?? "0"` the natural spelling AND the correct one — the shell
// side has to say `{{ index .version "ts" | default "0" }}` for the same
// reason, and gets it wrong more often.
func slotEnv(slot Slot, in Input) map[string]any {
	env := map[string]any{"source": orEmpty(in.Source)}

	switch slot {
	case SlotCheck:
		env["version"] = orEmpty(in.Version)
	case SlotIn:
		env["version"] = orEmpty(in.Version)
		env["params"] = orEmpty(in.Params)
	case SlotOut:
		env["params"] = orEmpty(in.Params)
	}

	return env
}

func orEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}

	return m
}

// slotFuncs returns the functions a slot may call. file() is out-only,
// because it reads the put's read view and no other slot has one.
func slotFuncs(ctx context.Context, slot Slot, in Input) []expr.Option {
	funcs := []expr.Option{
		expr.Function("env", envFunc(in.EnvAllow)),
		expr.Function("http", httpFunc(ctx)),
		expr.Function("fail", failFunc()),
	}

	if slot == SlotOut {
		funcs = append(funcs, expr.Function("file", fileFunc(in.Dir)))
	}

	return funcs
}

// slotBuiltinBans removes expr's date builtins from check and in.
//
// A version has to be a stable identity — it is the cache key, the dedupe
// key, and what a `passed:` constraint matches on — so a check that returns
// now() produces a new "version" on every poll and re-runs the pipeline
// forever. in is banned for the neighbouring reason: its output is reused
// across builds through the resource cache (see merkle.ResourceCacheKey), so
// it must be a pure function of the version it fetches.
//
// out keeps them. A published message may legitimately carry a timestamp,
// and a shell out: can already call date.
func slotBuiltinBans(slot Slot) []expr.Option {
	if slot == SlotOut {
		return nil
	}

	return []expr.Option{
		expr.DisableBuiltin("now"),
		expr.DisableBuiltin("date"),
		expr.DisableBuiltin("duration"),
		expr.DisableBuiltin("timezone"),
	}
}

// run compiles and evaluates one slot, returning the raw result.
//
// Idle connections are released when the expression finishes rather than
// held: a watch loop polls minutes apart, so a kept-alive socket is more
// likely to be a dead one the next poll has to notice than a saved
// handshake. It also leaves the package quiet enough for a goroutine leak
// check to be meaningful.
func run(ctx context.Context, slot Slot, src string, in Input) (any, error) {
	defer CloseIdleConnections()

	program, env, err := compileProgram(ctx, slot, src, in)
	if err != nil {
		return nil, err
	}

	result, err := expr.Run(program, env)
	if err != nil {
		return nil, fmt.Errorf("expr.%s: %w", slot, err)
	}

	return result, nil
}

// RunCheck evaluates a check expression and returns the versions it produced,
// oldest first — the same contract, and the same caller responsibility for
// ordering, as a shell check's stdout.
func RunCheck(ctx context.Context, src string, in Input) ([]map[string]any, error) {
	result, err := run(ctx, SlotCheck, src, in)
	if err != nil {
		return nil, err
	}

	items, ok := result.([]any)
	if !ok {
		// A check returning a bare map is the likely mistake — one version
		// instead of a list of them — so say what was wanted rather than
		// printing a Go type.
		return nil, fmt.Errorf("expr.check: expression returned %T, want an array of version objects (oldest first)", result)
	}

	versions := make([]map[string]any, 0, len(items))

	for i, item := range items {
		version, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expr.check: version %d is %T, want an object", i, item)
		}

		versions = append(versions, version)
	}

	return versions, nil
}

// RunIn evaluates an in expression and returns the files it produced, as a
// map of artifact-relative path to contents.
//
// Returning a file map rather than handing the language a write() builtin is
// what keeps an expression pure: no side effects means the result is a
// function of its inputs, which is the property the resource cache is
// already built on. The caller writes the files (see resource.exprRunIn),
// and is also where the path guard lives.
func RunIn(ctx context.Context, src string, in Input) (map[string]string, error) {
	result, err := run(ctx, SlotIn, src, in)
	if err != nil {
		return nil, err
	}

	entries, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expr.in: expression returned %T, want an object of relative path to file contents", result)
	}

	files := make(map[string]string, len(entries))

	for path, value := range entries {
		text, ok := value.(string)
		if !ok {
			// Not marshaled automatically: guessing would turn a mistyped
			// expression into a file full of JSON nobody asked for. toJSON is
			// one call, and says so at the call site.
			return nil, fmt.Errorf("expr.in: file %q is %T, want a string (wrap it in toJSON() if it is an object)", path, value)
		}

		files[path] = text
	}

	return files, nil
}

// RunOut evaluates an out expression and returns the version it published, or
// nil when it published nothing versionable — the same tolerance a shell
// out: gets for printing no JSON.
func RunOut(ctx context.Context, src string, in Input) (map[string]any, error) {
	result, err := run(ctx, SlotOut, src, in)
	if err != nil {
		return nil, err
	}

	if result == nil {
		return nil, nil //nolint:nilnil // "published, but produced no version" is a real outcome, as it is for a shell out:
	}

	version, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expr.out: expression returned %T, want an object (the version published) or nil", result)
	}

	return version, nil
}

// errUnsetEnv is returned by env() for a name that is allowed but not set, so
// callers can tell "you forgot to export it" from "you did not allow it".
var errUnsetEnv = errors.New("is not set in the environment")
