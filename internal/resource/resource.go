// Package resource runs a resource type's check/in/out shell commands and
// selects among the versions a check returns.
package resource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// CheckVersions renders rt.Config.Check against {"source": source,
// "version": version}, runs it, and parses stdout as a JSON array into
// []map[string]any. Ordering (oldest-first) is entirely the check command's
// responsibility, per Concourse convention — no sorting happens here (see
// docs/conformance.md and internal/resource/resource_test.go's
// TestSelectVersion/"latest when unpinned"; Concourse doc:
// concourse-ci.org/docs/resource-types/implementing/, "check" section).
//
// version is the last version this pipeline recorded for the resource —
// Concourse's "current version", which lets a check ask its API for exactly
// what it has not seen (Slack's oldest:, GitHub's since:) instead of guessing
// a window. It is nil-normalized to an empty map HERE, the single place both
// backends and every call path agree on, so the first-ever check renders
// against a present-but-empty map rather than a missing key. Templates render
// with missingkey=error, so an optional cursor field is spelled
// {{ index .version "ts" | default "0" }} — the same shape an optional
// source: field or get param: already uses.
//
// Only steps watch advances the cursor (see internal/trigger's pollOnce,
// which records after a successful check AND enqueue, so a failed poll never
// advances past items nobody saw). The run and plan paths read it and never
// write it.
//
// extraEnv is the resource INSTANCE's own env: (config.Resource.Env) —
// additional names unioned with the resource TYPE's env: for this call only,
// so one resource of a shared type can read a credential the type itself
// doesn't name. See config.Resource.Env for why this is additive, never a
// replacement.
//
// When rt.Config.MCP is set, this calls its check: tool instead (see
// mcpCheckVersions) — cfg is needed only for that path, to resolve the
// referenced mcp_servers: entry; the shell path below ignores it, so a nil
// cfg is fine whenever the caller knows rt isn't mcp-backed.
func CheckVersions(
	ctx context.Context, cfg *config.Config, rt config.ResourceType, extraEnv []string, source, version map[string]any,
) ([]map[string]any, error) {
	if version == nil {
		version = map[string]any{}
	}

	switch rt.Config.Backend() {
	case config.BackendMCP:
		return mcpCheckVersions(ctx, cfg, rt, source, version)
	case config.BackendExpr:
		return exprCheckVersions(ctx, rt, extraEnv, source, version)
	case config.BackendShell:
	}

	events.Logger(ctx).Debug("resource.check", "resource_type", rt.Name, "source", source, "version", version)

	command, err := template.Render(rt.Config.Check, map[string]any{"source": source, "version": version})
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Env: UnionEnv(rt.Env, extraEnv), User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()})
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name + " check")
	defer shell.CloseRunner(runner, rt.Name+" check")

	out, err := runner.RunCapture(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	// UseNumber, for the reason the mcp path (decodeMapSlice) and
	// ParseVersionJSON already have it: a version field goes straight back
	// out — into in:'s template, into a put's payload, into an API's query
	// string. encoding/json's default float64 turns an id of
	// 1234567890123456789 into 1.2345678901234568e+18 and a fractional
	// timestamp into exponent notation, so the value the resource is asked
	// about is not the value it reported.
	//
	// A version is an identity, and this is the path that mints one.
	versions, err := decodeVersionArray(out)
	if err != nil {
		events.Logger(ctx).Debug("resource.check", "resource_type", rt.Name, "output", string(out))

		return nil, fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
	}

	events.Logger(ctx).Info("resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// UnionEnv combines a resource type's own env: with one resource instance's
// additional env: (config.Resource.Env), without mutating either slice —
// both are shared config data that later calls (or other resources of the
// same type) still read.
//
// Exported so internal/merkle can fold the identical union into a get/put
// node's hashed content — the env NAMES are cache identity for the same
// reason resourceType.Env alone already was (see merkle.GetNodeContent),
// and the two packages must agree on what that union is.
// A name repeated between the two lists appears once, in first-occurrence
// order. It IS a union, and a resource restating a name its type already
// allows means nothing new — but a duplicate would otherwise reach
// shell.RunnerSpec.Env twice and, worse, hash differently from the identical
// effective allow-list without the repeat, missing the merkle cache for no
// semantic change.
func UnionEnv(typeEnv, extraEnv []string) []string {
	if len(extraEnv) == 0 {
		return typeEnv
	}

	union := make([]string, 0, len(typeEnv)+len(extraEnv))
	seen := make(map[string]bool, len(typeEnv)+len(extraEnv))

	for _, name := range slices.Concat(typeEnv, extraEnv) {
		if seen[name] {
			continue
		}

		seen[name] = true

		union = append(union, name)
	}

	return union
}

// VersionMode returns a get step's version selection mode ("latest",
// "every", or "pinned" with its pinned map), per Concourse's get.version
// field convention: unset or the string "latest" means latest, the string
// "every" means every version, and a map means pinned to a specific version.
func VersionMode(step config.Step) (mode string, pinned map[string]string) {
	// The one mode config also has to recognize (to reject it where it cannot
	// fan out — see validateVersionEvery), so both read the same predicate
	// rather than each spelling the comparison.
	if step.VersionEvery() {
		return "every", nil
	}

	switch v := step.Version.(type) {
	case string:
		return "latest", nil
	case map[string]any:
		m := make(map[string]string, len(v))
		for k, val := range v {
			m[k] = fmt.Sprint(val)
		}

		return "pinned", m
	case map[string]string:
		return "pinned", v
	default:
		return "latest", nil
	}
}

// versionsFor returns the versions a get step is choosing among: the ones the
// caller supplied, or the ones a fresh check reports.
//
// A pin is answered by a check even when versions were supplied. It is an
// instruction, not a question — `--pin ref=abc123` on a resource that has
// since moved on must still find abc123, and would not if it were matched
// against the slice a poll happened to observe. Cache.unconsumed exempts
// pinned runs for the same reason.
func versionsFor(
	ctx context.Context, cfg *config.Config, step config.Step,
	res *config.Resource, resourceType *config.ResourceType,
	cliPinned map[string]string, supplied func(resourceName string) []map[string]any,
) ([]map[string]any, error) {
	mode, _ := VersionMode(step)

	if supplied != nil && len(cliPinned) == 0 && mode != "pinned" {
		// nil means nobody supplied any; a non-nil empty slice means the
		// caller resolved none, and is honored rather than re-derived.
		if versions := supplied(res.Name); versions != nil {
			slog.Debug("resource.versions_supplied", "resource", res.Name, "versions", len(versions))

			return versions, nil
		}
	}

	versions, err := CheckVersions(ctx, cfg, *resourceType, res.Env, res.Source, nil)
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	return versions, nil
}

// SelectVersion returns the version matching all key/values in pinned, or
// (if pinned is empty) the last element of versions (latest, by convention).
func SelectVersion(versions []map[string]any, pinned map[string]string) (map[string]any, error) {
	slog.Debug("resource.select_version", "versions", len(versions), "pinned", pinned)

	if len(versions) == 0 {
		return nil, errors.New("no versions available")
	}

	if len(pinned) == 0 {
		version := versions[len(versions)-1]
		slog.Debug("resource.select_version", "version", version, "reason", "latest")

		return version, nil
	}

	for _, version := range versions {
		if matchesPin(version, pinned) {
			slog.Debug("resource.select_version", "version", version, "reason", "pinned", "pinned", pinned)

			return version, nil
		}
	}

	return nil, fmt.Errorf("no version matches pin %v", pinned)
}

func matchesPin(version map[string]any, pinned map[string]string) bool {
	for key, want := range pinned {
		got, ok := version[key]
		if !ok {
			return false
		}

		if fmt.Sprint(got) != want {
			return false
		}
	}

	return true
}

// RunIn renders rt.Config.In against {"source": source, "version": version,
// "params": params} and executes it with cwd = destDir (caller ensures
// destDir exists).
//
// params is the get step's own params:, mirroring Concourse, where a get's
// params tell the resource HOW to fetch — git's depth:/submodules:, s3's
// unpack: (concourse-ci.org/docs/steps/get/; see docs/conformance.md). It is
// nil for a get that declares none, which renders as an empty map so
// {{ .params.x }} on an unset key is empty rather than an error — the same
// treatment out: already gives a put with no params:.
//
// extraEnv is the resource instance's own env: — see CheckVersions' doc
// comment.
//
// When rt.Config.MCP is set, this calls mcpRunIn instead — see its doc
// comment for the in:-omitted default (writing version.json) and the
// materialization convention when in: is set.
func RunIn(ctx context.Context, cfg *config.Config, rt config.ResourceType, extraEnv []string, source, version, params map[string]any, destDir string) error {
	switch rt.Config.Backend() {
	case config.BackendMCP:
		return mcpRunIn(ctx, cfg, rt, source, version, params, destDir)
	case config.BackendExpr:
		return exprRunIn(ctx, rt, extraEnv, source, version, params, destDir)
	case config.BackendShell:
	}

	events.Logger(ctx).Debug("resource.in", "resource_type", rt.Name, "source", source, "version", version, "params", params, "dest_dir", destDir)

	command, err := template.Render(rt.Config.In, map[string]any{"source": source, "version": version, "params": params})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: destDir, Env: UnionEnv(rt.Env, extraEnv), User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name + " in")
	defer shell.CloseRunner(runner, rt.Name+" in")

	err = runner.Run(ctx, command)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	events.Logger(ctx).Info("resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}

// RunOut renders rt.Config.Out against {"source": source, "params": params}
// and executes it with cwd = srcDir. If stdout parses as a JSON object it's
// returned as result (loosely mirroring check's convention of emitting the
// version produced); unparsable or empty stdout is not an error — result
// is simply nil, since many out scripts won't emit anything (see
// docs/conformance.md and TestConformanceRunOutUnparsableStdoutIsNilNotError
// in resource_test.go).
//
// extraEnv is the resource instance's own env: — see CheckVersions' doc
// comment.
//
// When rt.Config.MCP is set, this calls mcpRunOut instead. rt.Config.MCP.Out
// is itself optional (see validateMCPResourcePuts, which rejects a put step
// targeting an mcp-backed type with no out: at load time), so this is only
// ever reached with it set.
func RunOut(ctx context.Context, cfg *config.Config, rt config.ResourceType, extraEnv []string, source, params map[string]any, srcDir string) (map[string]any, error) {
	switch rt.Config.Backend() {
	case config.BackendMCP:
		return mcpRunOut(ctx, cfg, rt, source, params, srcDir)
	case config.BackendExpr:
		return exprRunOut(ctx, rt, extraEnv, source, params, srcDir)
	case config.BackendShell:
	}

	events.Logger(ctx).Debug("resource.out", "resource_type", rt.Name, "source", source, "params", params, "src_dir", srcDir)

	command, err := template.Render(rt.Config.Out, map[string]any{"source": source, "params": params})
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: rt.Image, Cwd: srcDir, Env: UnionEnv(rt.Env, extraEnv), User: rt.User, Network: rt.Network,
		Privileged: rt.Privileged, CPUShares: rt.Limits.CPUShares(), MemoryBytes: rt.Limits.MemoryBytes()})
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name + " out")
	defer shell.CloseRunner(runner, rt.Name+" out")

	out, err := runner.RunCapture(ctx, command)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	var result map[string]any

	unmarshalErr := json.Unmarshal(out, &result)
	if unmarshalErr != nil {
		slog.Debug("resource.out", "resource_type", rt.Name, "output", string(out), "parse_error", unmarshalErr)

		return nil, nil //nolint:nilnil // unparsable/empty stdout is not an error; nil result means "no version produced"
	}

	events.Logger(ctx).Info("resource.put", "resource_type", rt.Name, "src_dir", srcDir, "result", result)

	return result, nil
}

// ResolveVersions determines which version(s) of a get step's resource to
// fetch. An explicit CLI pin always wins (a single version); otherwise the
// step's own version: field decides between latest (default, a single
// version), every (all versions returned by check), or a YAML-pinned
// version. Both the merkle planner and the executor call ResolveVersions so
// plan-time hashing and run-time execution stay in lockstep.
//
// supplied lets a caller hand over versions it has already resolved, in which
// case the check is not run at all. `steps watch` does this: its poll asked
// the resource what was new — precisely, using the cursor — and asking again
// here would answer a different question (see WithResolvedVersions).
//
// It is a callback rather than a value because this is the only place that
// knows the RESOLVED resource name: a get may alias it via `resource:`, and
// the caller holds the versions under the real name. The same reason
// WithConsumed is a callback, and the same reason both keep this package free
// of any dependency on the store.
//
// A nil callback, or one returning nil, means nothing was supplied and the
// check runs as it always has.
func ResolveVersions(
	ctx context.Context, cfg *config.Config, step config.Step, cliPinned map[string]string,
	supplied func(resourceName string) []map[string]any,
) (*config.Resource, *config.ResourceType, []map[string]any, error) {
	res, err := cfg.FindResource(step.GetResourceName())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	resourceType, err := cfg.FindResourceType(res.Type)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	versions, err := versionsFor(ctx, cfg, step, res, resourceType, cliPinned, supplied)
	if err != nil {
		return nil, nil, nil, err
	}

	// A CLI --version pin always wins; otherwise the step's own version:
	// field decides. version:every is the only path that fans out to more
	// than one version — every other path narrows to a single pin (an empty
	// pin meaning "latest").
	pin := cliPinned
	if len(pin) == 0 {
		mode, stepPinned := VersionMode(step)
		slog.Debug("resource.version_mode", "resource", res.Name, "mode", mode)

		if mode == "every" {
			return res, resourceType, versions, nil
		}

		pin = stepPinned
	}

	version, err := SelectVersion(versions, pin)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	return res, resourceType, []map[string]any{version}, nil
}
