// Package resource runs a resource type's check/in/out shell commands and
// selects among the versions a check returns.
package resource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/template"
)

// CheckVersions renders c.Type.Config.Check against {"source": source,
// "version": c.Version}, runs it, and parses stdout as a JSON array into
// []map[string]any. Ordering (oldest-first) is entirely the check command's
// responsibility, per Concourse convention — no sorting happens here (see
// docs/conformance.md and internal/resource/resource_test.go's
// TestSelectVersion/"latest when unpinned"; Concourse doc:
// concourse-ci.org/docs/resource-types/implementing/, "check" section).
//
// c.Version is the last version this pipeline recorded for the resource —
// Concourse's "current version", which lets a check ask its API for exactly
// what it has not seen (Slack's oldest:, GitHub's since:) instead of guessing
// a window. It is nil-normalized to an empty map HERE, the single place both
// backends and every call path agree on, so the first-ever check renders
// against a present-but-empty map rather than a missing key. Templates render
// with missingkey=error, so an optional cursor field is spelled
// {{ index .version "ts" | default "0" }} — the same shape an optional
// source: field or get param: already uses.
//
// Only the trigger poll loop advances the cursor (see internal/trigger's pollOnce,
// which records after a successful check AND enqueue, so a failed poll never
// advances past items nobody saw). The run and plan paths read it and never
// write it.
//
// c.Resource.Env is the resource INSTANCE's own env: —
// additional names unioned with the resource TYPE's env: for this call only,
// so one resource of a shared type can read a credential the type itself
// doesn't name. See config.Resource.Env for why this is additive, never a
// replacement.
//
// The backend is chosen once, by backendFor; mcp is the one that reads
// c.Config, to resolve the referenced mcp_servers: entry.
func CheckVersions(ctx context.Context, c Call) ([]map[string]any, error) {
	if c.Version == nil {
		c.Version = map[string]any{}
	}

	ctx, err := withResourceSecrets(ctx, c.Resource)
	if err != nil {
		return nil, err
	}

	ctx, err = WithTypePullLogin(ctx, c.Type)
	if err != nil {
		return nil, err
	}

	// The one place every check path (poll, refresh, a get's own check) reaches, so the limit holds for all of them.
	limit := c.Resource.CheckLimit()
	parent := ctx

	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()

	versions, err := backendFor(c.Type).check(ctx, c)
	if err != nil && ctx.Err() != nil && parent.Err() == nil {
		err = fmt.Errorf("check of resource %q exceeded check_timeout %s and was cancelled: %w", c.Resource.Name, limit, err)
	}

	return versions, redact.SecretsFrom(ctx).Error(err) //nolint:wrapcheck // masking keeps the chain (Unwrap); the backend already named the stage
}

// runnerSpec is how every shell stage of rt runs, before the stage adds its
// own tree: a check has none.
func runnerSpec(rt config.ResourceType, extraEnv []string) shell.RunnerSpec {
	iso := rt.Isolation()

	return shell.RunnerSpec{Image: rt.Image, Env: config.UnionEnv(rt.Env, extraEnv), User: iso.User, Network: iso.Network, ExtraHosts: iso.ExtraHosts, Sandbox: iso.Sandbox,
		Privileged: iso.Privileged, CPUShares: iso.Limits.CPUShares(), MemoryBytes: iso.Limits.MemoryBytes()}
}

// keyNames is what a debug line may say about a source or params map: their values carry credentials (see template.Render).
func keyNames(m map[string]any) []string { return slices.Sorted(maps.Keys(m)) }

type shellBackend struct{}

func (shellBackend) check(ctx context.Context, c Call) ([]map[string]any, error) {
	rt, source, version := c.Type, c.Resource.Source, c.Version
	slog.DebugContext(ctx, "resource.check", "resource_type", rt.Name, "source_keys", keyNames(source), "version", version)

	command, err := template.Render(rt.Config.Check, map[string]any{"source": source, "version": version})
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	spec := runnerSpec(rt, c.Resource.Env)

	share, key := sharedCheckFor(ctx, command, spec)

	// Never shared: two pipelines can hold the same check and resolve the same names to different credentials, and the shared answer would be the other pipeline's.
	if share != nil && len(c.Resource.Secrets) == 0 {
		return share.do(ctx, key, func() ([]map[string]any, error) {
			return runShellCheck(ctx, rt, spec, command)
		})
	}

	return runShellCheck(ctx, rt, spec, command)
}

// runShellCheck runs a rendered check and parses what it printed.
func runShellCheck(ctx context.Context, rt config.ResourceType, spec shell.RunnerSpec, command string) ([]map[string]any, error) {
	runner, err := newRunner(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	runner = runner.WithLabel(rt.Name + " check")
	defer shell.CloseRunner(runner, rt.Name+" check")

	// A check is not part of a build, and a run id in a version would mint one per run.
	out, err := shell.RunCapture(shell.WithBuildMetadata(ctx, shell.BuildMetadata{}), runner, command)
	if err != nil {
		return nil, fmt.Errorf("check %q: %w", rt.Name, err)
	}

	versions, err := DecodeVersions(out)
	if err != nil {
		slog.DebugContext(ctx, "resource.check", "resource_type", rt.Name, "output_bytes", len(out))

		return nil, fmt.Errorf("check %q: could not parse JSON output: %w", rt.Name, err)
	}

	slog.InfoContext(ctx, "resource.checked", "resource_type", rt.Name, "versions", len(versions))

	return versions, nil
}

// Mode is how a get step picks among its resource's versions.
type Mode string

// The modes, spelled as get.version: spells them.
const (
	ModeLatest Mode = "latest"
	ModeEvery  Mode = "every"
	ModePinned Mode = "pinned"
)

// VersionMode returns how a get step picks its version, and the pin when that
// is ModePinned, per Concourse's get.version field convention: unset or
// the string "latest" means latest, the string "every" means every version,
// and a map means pinned to a specific version.
func VersionMode(step config.Step) (mode Mode, pinned map[string]string) {
	// The one mode config also has to recognize (to reject it where it cannot
	// fan out — see validateVersionEvery), so both read the same predicate
	// rather than each spelling the comparison.
	if step.VersionEvery() {
		return ModeEvery, nil
	}

	switch v := step.Version.(type) {
	case string:
		return ModeLatest, nil
	case map[string]any:
		m := make(map[string]string, len(v))
		for k, val := range v {
			m[k] = fmt.Sprint(val)
		}

		return ModePinned, m
	case map[string]string:
		return ModePinned, v
	default:
		return ModeLatest, nil
	}
}

// versionsFor returns the versions a get step is choosing among: the ones the
// caller supplied, or the ones a fresh check reports.
//
// A pin is answered from the supplied versions only when one of them matches
// it, and by a check otherwise — Concourse's pinned resolver reads version
// history, and its check runs from the pin to put a missing one there. The
// history comes first because some resources cannot be asked again: a
// webhook's check reports nothing, so its deliveries exist only as recorded
// (#274). The check comes second because a pin is an instruction, not a
// question — `--pin ref=abc123` on a resource that has since moved on must
// still find abc123, which the slice a poll happened to observe may not hold.
// Cache.unconsumed exempts pinned runs for the same reason.
func versionsFor(
	ctx context.Context, cfg *config.Config, step config.Step,
	res *config.Resource, resourceType *config.ResourceType,
	cliPinned map[string]string, supplied func(resourceName string) []map[string]any,
) ([]map[string]any, error) {
	_, pin := VersionMode(step)
	if len(cliPinned) > 0 {
		pin = cliPinned
	}

	if supplied != nil {
		// nil means nobody supplied any; a non-nil empty slice means the
		// caller resolved none, and is honored rather than re-derived.
		versions := supplied(res.Name)
		holdsPin := len(pin) == 0 || slices.ContainsFunc(versions, func(v map[string]any) bool { return matchesPin(v, pin) })

		if versions != nil && holdsPin {
			slog.DebugContext(ctx, "resource.versions_supplied", "resource", res.Name, "versions", len(versions))

			return versions, nil
		}
	}

	// The check runs where the RESOURCE says, as every other check of it
	// does — a source only reachable from a worker has to be asked from
	// there — while the step's own tags: govern only its in:.
	ctx, err := Place(ctx, config.Step{Get: res.Name, Tags: res.Tags})
	if err != nil {
		return nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	versions, err := CheckVersions(ctx, Call{Config: cfg, Type: *resourceType, Resource: *res})
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

// MatchesPin reports whether version carries every pinned field with the
// pinned value, compared as text.
func MatchesPin(version map[string]any, pinned map[string]string) bool {
	return matchesPin(version, pinned)
}

// pinFor is the CLI pin as it applies to one resource: the pin itself when
// some version carries every field it names (or none is known yet, so a check
// has to say), and nil when the versions are of another shape, which makes the
// pin another resource's. A field that is carried and never equal is still
// this resource's pin, and fails as "no version matches pin".
func pinFor(versions []map[string]any, pin map[string]string) map[string]string {
	if len(pin) == 0 || len(versions) == 0 {
		return pin
	}

	carries := func(version map[string]any) bool {
		for field := range pin {
			if _, ok := version[field]; !ok {
				return false
			}
		}

		return true
	}

	if slices.ContainsFunc(versions, carries) {
		return pin
	}

	return nil
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

// RunIn renders c.Type.Config.In against {"source": source, "version":
// c.Version, "params": c.Params} and executes it with cwd = c.Dir (caller
// ensures it exists).
//
// c.Params is the get step's own params:, mirroring Concourse, where a get's
// params tell the resource HOW to fetch — git's depth:/submodules:, s3's
// unpack: (concourse-ci.org/docs/steps/get/; see docs/conformance.md). It is
// nil for a get that declares none, which renders as an empty map so
// {{ .params.x }} on an unset key is empty rather than an error — the same
// treatment out: already gives a put with no params:.
//
// c.Resource.Env is the resource instance's own env: — see CheckVersions'
// doc comment.
//
// An mcp-backed type goes to mcpRunIn — see its doc comment for the
// in:-omitted default (writing version.json) and the materialization
// convention when in: is set.
func RunIn(ctx context.Context, c Call) error {
	ctx, err := withResourceSecrets(ctx, c.Resource)
	if err != nil {
		return err
	}

	ctx, err = WithTypePullLogin(ctx, c.Type)
	if err != nil {
		return err
	}

	// Masked where it is born: a failed stage's error climbs into the step's and the run's record and the scheduler's log, none of which carries the run's secrets.
	return redact.SecretsFrom(ctx).Error(backendFor(c.Type).in(ctx, c)) //nolint:wrapcheck // masking keeps the chain (Unwrap); the backend already named the stage
}

func (shellBackend) in(ctx context.Context, c Call) error {
	rt, source, version, params, destDir := c.Type, c.Resource.Source, c.Version, c.Params, c.Dir
	slog.DebugContext(ctx, "resource.in", "resource_type", rt.Name, "source_keys", keyNames(source), "version", version, "param_keys", keyNames(params), "dest_dir", destDir)

	command, err := template.Render(rt.Config.In, map[string]any{"source": source, "version": version, "params": params})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	// FetchAll: the directory IS the output, so a worker sends the whole of
	// it home — there is no declared outputs: list to name a subset.
	spec := runnerSpec(rt, c.Resource.Env)
	spec.Cwd, spec.Workdir, spec.FetchAll = destDir, shell.GetDir, true

	runner, err := newRunner(ctx, spec)
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	// Unlabeled: the caller prefixes what it streams, so a recorder between them keeps the script's own bytes.
	defer shell.CloseRunner(runner, rt.Name+" in")

	// Captured as well as streamed, bounded, for an answer printed in Concourse's shape; anything longer is no answer.
	res, err := runner.Exec(ctx, shell.Command{Script: command, Stdin: !events.Headless(ctx), Stream: shell.StreamBoth, Capture: true, Limit: maxInAnswerBytes, Reattach: true})
	if err != nil {
		return fmt.Errorf("in %q: %w", rt.Name, err)
	}

	reportInAnswer(ctx, res.Stdout)

	slog.InfoContext(ctx, "resource.fetched", "resource_type", rt.Name, "dest_dir", destDir)

	return nil
}

// PutInputs is what a put's expr out: may read of its build through
// version(): its input names, and the versions this build's gets fetched for
// them, by get name. The shell and mcp backends ignore it.
type PutInputs struct {
	Names    []string
	Versions map[string]map[string]any
}

// RunOut renders c.Type.Config.Out against {"source": source, "params":
// c.Params} and executes it with cwd = c.Dir. If stdout parses as a JSON
// object it's returned as result (loosely mirroring check's convention of
// emitting the version produced); unparsable or empty stdout is not an error
// — result is simply nil, since many out scripts won't emit anything (see
// docs/conformance.md and TestConformanceRunOutUnparsableStdoutIsNilNotError
// in resource_test.go).
//
// c.Resource.Env is the resource instance's own env: — see CheckVersions'
// doc comment.
//
// An mcp-backed type goes to mcpRunOut. Its out: is itself optional (see
// validateMCPResourcePuts, which rejects a put step targeting an mcp-backed
// type with no out: at load time), so this is only ever reached with it set.
func RunOut(ctx context.Context, c Call) (map[string]any, error) {
	ctx, err := withResourceSecrets(ctx, c.Resource)
	if err != nil {
		return nil, err
	}

	ctx, err = WithTypePullLogin(ctx, c.Type)
	if err != nil {
		return nil, err
	}

	version, err := backendFor(c.Type).out(ctx, c)
	if err == nil && answersWrapped(c.Type.Config.Backend()) {
		if inner, facts, wrapped := unwrapAnswer(version); wrapped {
			version = inner

			reportFacts(ctx, facts)
		}
	}

	return version, redact.SecretsFrom(ctx).Error(err) //nolint:wrapcheck // masking keeps the chain (Unwrap); the backend already named the stage
}

func (shellBackend) out(ctx context.Context, c Call) (map[string]any, error) {
	rt, source, params, srcDir := c.Type, c.Resource.Source, c.Params, c.Dir
	slog.DebugContext(ctx, "resource.out", "resource_type", rt.Name, "source_keys", keyNames(source), "param_keys", keyNames(params), "src_dir", srcDir)

	command, err := template.Render(rt.Config.Out, map[string]any{"source": source, "params": params})
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	// Nothing fetched back: an out publishes elsewhere and answers on stdout.
	spec := runnerSpec(rt, c.Resource.Env)
	spec.Cwd, spec.Workdir = srcDir, shell.PutDir

	runner, err := newRunner(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	// Unlabeled: the caller prefixes what it streams, so a recorder between them keeps the script's own bytes.
	defer shell.CloseRunner(runner, rt.Name+" out")

	out, err := shell.RunCaptureReattachable(ctx, runner, command)
	if err != nil {
		return nil, fmt.Errorf("out %q: %w", rt.Name, err)
	}

	result, unmarshalErr := DecodeVersion(out)
	if unmarshalErr != nil {
		slog.DebugContext(ctx, "resource.out", "resource_type", rt.Name, "output_bytes", len(out), "parse_error", unmarshalErr)

		return nil, nil //nolint:nilnil // unparsable/empty stdout is not an error; nil result means "no version produced"
	}

	slog.InfoContext(ctx, "resource.put", "resource_type", rt.Name, "src_dir", srcDir, "result", result)

	return result, nil
}

// versionsUnderPin is versionsFor with the CLI pin narrowed to this get, the
// pin as it applies here, and whether a CLI pin passed this get over as
// another resource's.
//
// A CLI pin names one resource's version by its fields, and a job may get
// several: `--pin number=87` is about the pull request, not the image beside
// it whose versions are {digest}. Asked of every get, it failed the image
// with "no version matches pin" before a step ran. So a get none of whose
// versions carry the pinned fields is not the one pinned. Judged on recorded
// versions first, so such a get is not sent to a check for a pin that was
// never its own (a webhook's check answers nothing), and only under a CLI
// pin: an unpinned get asks for its versions once.
func versionsUnderPin(
	ctx context.Context, cfg *config.Config, step config.Step,
	res *config.Resource, resourceType *config.ResourceType,
	cliPinned map[string]string, supplied func(resourceName string) []map[string]any,
) ([]map[string]any, map[string]string, bool, error) {
	var known []map[string]any
	if supplied != nil && len(cliPinned) > 0 {
		known = supplied(res.Name)
	}

	pin := pinFor(known, cliPinned)

	versions, err := versionsFor(ctx, cfg, step, res, resourceType, pin, supplied)
	if err != nil {
		return nil, nil, false, err
	}

	pin = pinFor(versions, pin)

	return versions, pin, len(cliPinned) > 0 && len(pin) == 0, nil
}

// ResolveVersions determines which version(s) of a get step's resource to
// fetch. An explicit CLI pin always wins (a single version); otherwise the
// step's own version: field decides between latest (default, a single
// version), every (all versions returned by check), or a YAML-pinned
// version. Both the merkle planner and the executor call ResolveVersions so
// plan-time hashing and run-time execution stay in lockstep.
//
// supplied lets a caller hand over versions it has already resolved, in which
// case the check is not run at all — unless a pin matches none of them (see
// versionsFor). `steps web` does this: its poll asked
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

	// passedOver: the CLI pin is another resource's. This get still takes ONE
	// version, its own version: pin or the latest: a pinned run builds a
	// single set, and everything downstream of here reads a CLI pin as that
	// promise.
	versions, pin, passedOver, err := versionsUnderPin(ctx, cfg, step, res, resourceType, cliPinned, supplied)
	if err != nil {
		return nil, nil, nil, err
	}

	// A CLI --version pin always wins; otherwise the step's own version:
	// field decides. version:every is the only path that fans out to more
	// than one version — every other path narrows to a single pin (an empty
	// pin meaning "latest").
	if len(pin) == 0 {
		mode, stepPinned := VersionMode(step)
		slog.DebugContext(ctx, "resource.version_mode", "resource", res.Name, "mode", mode, "pin_passed_over", passedOver)

		if mode == ModeEvery && !passedOver {
			return res, resourceType, versions, nil
		}

		pin = stepPinned
	}

	version, err := SelectVersion(versions, pin)
	if err != nil {
		// Named because a passed: gate holding every version back reads as a
		// broken check otherwise, and it is the first thing anyone hits.
		if len(step.Passed) > 0 && len(versions) == 0 {
			return nil, nil, nil, fmt.Errorf("get %q: no version of %s has passed %v yet: %w", step.Get, res.Name, step.Passed, err)
		}

		return nil, nil, nil, fmt.Errorf("get %q: %w", step.Get, err)
	}

	return res, resourceType, []map[string]any{version}, nil
}
