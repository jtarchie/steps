package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync/atomic"
)

// WorkspaceProvider is built once per CLI invocation from cfg.Workspace (nil
// meaning the feature is off, in which case newWorkspaceProvider returns
// sharedProvider — today's single-mutable-directory behavior, unchanged). It
// owns whatever on-disk root its strategy needs and fails fast, at startup,
// on anything that would only surface as a confusing mid-build error
// otherwise (wrong platform, wrong filesystem, missing binaries).
type WorkspaceProvider interface {
	Validate() error
	// NewBuild creates the workspace for one triggered build (see
	// runTriggeredBuild) — or, called once from RunJob, the job-level build
	// that steps preceding any get run in. Each call is fully independent:
	// it never shares state with a build from a previous call, matching
	// today's per-triggered-build isolation (see runTriggeredBuild's doc).
	NewBuild(ctx context.Context, label string) (BuildWorkspace, error)
	Close() error
}

// BuildWorkspace is one triggered build's filesystem view: where get steps
// land pristine resource contents, and where task/agent/put steps draw
// their per-step workspace from.
type BuildWorkspace interface {
	// ResourceDir returns the (empty, freshly created) directory a get step
	// should fetch resource name into.
	ResourceDir(ctx context.Context, name string) (string, error)
	// TaskSpace materializes a task or agent step's working directory: the
	// shared implementation always returns the build root regardless of
	// inputs/outputs (today's behavior); an isolating implementation returns
	// a directory containing only an <input>/ copy or snapshot of each named
	// input plus an empty <output>/ directory for each named output.
	TaskSpace(ctx context.Context, label string, inputs, outputs []string) (StepSpace, error)
	// PutSpace composes a put step's read view from its declared inputs.
	// Unlike TaskSpace/agent steps, a put step never has outputs of its own.
	PutSpace(ctx context.Context, label string, inputs []string) (StepSpace, error)
	Close() error
}

// StepSpace is one step's materialized working directory.
type StepSpace interface {
	Dir() string
	// Capture persists the step's declared outputs back into the build's
	// artifact store, so later steps can name them as inputs. A no-op for
	// the shared implementation. Called only after the step itself
	// succeeded.
	Capture(ctx context.Context) error
	// Close removes anything TaskSpace/PutSpace created for this step. A
	// no-op for the shared implementation.
	Close() error
}

// artifactNamePattern constrains input/output/resource names used to build
// filesystem paths: this is load-bearing, not cosmetic — it rules out `..`
// segments and separators, so a name can never escape the directory it's
// joined under, and keeps the copy/btrfs backends' shelled-out argv
// construction free of characters that would need escaping.
var artifactNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func validateArtifactName(name string) error {
	if !artifactNamePattern.MatchString(name) {
		return fmt.Errorf("invalid artifact name %q: must match %s", name, artifactNamePattern.String())
	}

	return nil
}

// labelSanitizePattern replaces anything outside this set with '_' when
// building a step directory name from a task/agent/put/resource name, so an
// unusual but otherwise-valid config name can never introduce a path
// separator or traversal segment into a generated directory name.
var labelSanitizePattern = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitizeLabel(label string) string {
	sanitized := labelSanitizePattern.ReplaceAllString(label, "_")
	if sanitized == "" {
		return "_"
	}

	return sanitized
}

// newWorkspaceProvider builds the provider a pipeline's workspace: block
// selects. ws == nil (no block present) is the default, unchanged behavior.
func newWorkspaceProvider(ws *WorkspaceConfig) (WorkspaceProvider, error) {
	if ws == nil {
		return &sharedProvider{}, nil
	}

	switch ws.Strategy {
	case "copy":
		return newCopyProvider(ws)
	case "btrfs":
		return newBtrfsProvider(ws), nil
	default:
		return nil, fmt.Errorf("unknown workspace strategy %q", ws.Strategy)
	}
}

// closeBuild and closeSpace are best-effort cleanup helpers shared by
// job.go/agent.go's deferred Close calls: a cleanup failure must never mask
// the original error already being returned, so it's only logged.
func closeBuild(bw BuildWorkspace, label string) {
	err := bw.Close()
	if err != nil {
		slog.Error("workspace.build_close", "label", label, "error", err)
	}
}

func closeSpace(space StepSpace, label string) {
	err := space.Close()
	if err != nil {
		slog.Error("workspace.space_close", "label", label, "error", err)
	}
}

// --- sharedProvider: today's single-mutable-directory behavior ---

// sharedProvider is the default WorkspaceProvider used when no workspace:
// block is configured. Every step in a build sees the same directory,
// exactly as before this feature existed.
type sharedProvider struct{}

func (*sharedProvider) Validate() error { return nil }

func (*sharedProvider) NewBuild(_ context.Context, _ string) (BuildWorkspace, error) {
	root, err := os.MkdirTemp("", "steps-*")
	if err != nil {
		return nil, fmt.Errorf("could not create workspace: %w", err)
	}

	slog.Debug("workspace.create", "dir", root)

	return &sharedBuild{root: root}, nil
}

func (*sharedProvider) Close() error { return nil }

type sharedBuild struct{ root string }

func (b *sharedBuild) ResourceDir(_ context.Context, name string) (string, error) {
	dir := filepath.Join(b.root, name)

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return "", fmt.Errorf("could not create resource dir %q: %w", dir, err)
	}

	return dir, nil
}

func (b *sharedBuild) TaskSpace(_ context.Context, _ string, _, _ []string) (StepSpace, error) {
	return sharedSpace{dir: b.root}, nil
}

func (b *sharedBuild) PutSpace(_ context.Context, _ string, _ []string) (StepSpace, error) {
	return sharedSpace{dir: b.root}, nil
}

func (b *sharedBuild) Close() error {
	slog.Debug("workspace.remove", "dir", b.root)

	err := os.RemoveAll(b.root)
	if err != nil {
		return fmt.Errorf("could not remove workspace %q: %w", b.root, err)
	}

	return nil
}

type sharedSpace struct{ dir string }

func (s sharedSpace) Dir() string                   { return s.dir }
func (sharedSpace) Capture(_ context.Context) error { return nil }
func (sharedSpace) Close() error                    { return nil }

// --- isolatingProvider: shared lifecycle over a pluggable treeBackend ---

// treeBackend abstracts the one thing that actually differs between the
// copy and btrfs strategies: how a directory is created empty, how one
// directory's contents are materialized into a fresh one (a recursive copy,
// or a CoW snapshot), and how a materialized tree is torn down.
// isolatingProvider/Build/Space implement the shared directory-layout and
// capture/cleanup lifecycle exactly once, over either backend.
type treeBackend interface {
	// createEmpty creates a fresh, empty directory at dir. dir's parent must
	// already exist.
	createEmpty(ctx context.Context, dir string) error
	// materialize copies or snapshots src's contents into a fresh directory
	// at dst (dst must not already exist; dst's parent must).
	materialize(ctx context.Context, src, dst string) error
	// remove tears down a single tree created by createEmpty/materialize
	// (one subvolume, for btrfs). It must not depend on ctx staying valid —
	// callers may invoke it during cleanup after ctx has already been
	// canceled (e.g. on SIGINT), so implementations that shell out use their
	// own bounded-lifetime context internally. remove is a no-op, not an
	// error, if dir does not exist.
	remove(dir string) error
	// removeTree removes a plain directory that may *contain* materialized
	// trees (nested subvolumes, for btrfs) — the per-step and per-build
	// wrapper directories, which are themselves plain dirs but hold the
	// backend's subvolumes. The btrfs backend deletes those subvolumes
	// explicitly first, so cleanup doesn't rely on os.RemoveAll's rmdir of a
	// subvolume, which restrictive mounts/older kernels deny. Like remove, it
	// is ctx-independent and a no-op if dir does not exist.
	removeTree(dir string) error
}

// isolatingProvider is the shared WorkspaceProvider for any strategy that
// materializes real per-step directories (copy, btrfs).
type isolatingProvider struct {
	backend  treeBackend
	validate func() error
	root     string
	ownsRoot bool
	builds   atomic.Int64
}

func (p *isolatingProvider) Validate() error {
	if p.validate != nil {
		return p.validate()
	}

	return nil
}

func (p *isolatingProvider) NewBuild(ctx context.Context, label string) (BuildWorkspace, error) {
	n := p.builds.Add(1)
	root := filepath.Join(p.root, fmt.Sprintf("b-%d-%s", n, sanitizeLabel(label)))

	err := os.MkdirAll(root, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create build workspace %q: %w", root, err)
	}

	artifacts := filepath.Join(root, "artifacts")

	err = os.MkdirAll(artifacts, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create artifact store %q: %w", artifacts, err)
	}

	steps := filepath.Join(root, "steps")

	err = os.MkdirAll(steps, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create step directory %q: %w", steps, err)
	}

	slog.Debug("workspace.create", "dir", root, "backend", "isolating")

	_ = ctx // no subprocess work happens at this layer; ctx kept for interface symmetry

	return &isolatingBuild{backend: p.backend, root: root, artifacts: artifacts, stepsDir: steps}, nil
}

func (p *isolatingProvider) Close() error {
	if !p.ownsRoot {
		return nil
	}

	err := os.RemoveAll(p.root)
	if err != nil {
		return fmt.Errorf("could not remove workspace root %q: %w", p.root, err)
	}

	return nil
}

type isolatingBuild struct {
	backend     treeBackend
	root        string
	artifacts   string
	stepsDir    string
	stepCounter atomic.Int64
}

func (b *isolatingBuild) ResourceDir(ctx context.Context, name string) (string, error) {
	err := validateArtifactName(name)
	if err != nil {
		return "", err
	}

	dir := filepath.Join(b.artifacts, name)

	err = b.backend.createEmpty(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("could not create resource dir %q: %w", dir, err)
	}

	return dir, nil
}

func (b *isolatingBuild) TaskSpace(ctx context.Context, label string, inputs, outputs []string) (StepSpace, error) {
	return b.newSpace(ctx, label, inputs, outputs)
}

func (b *isolatingBuild) PutSpace(ctx context.Context, label string, inputs []string) (StepSpace, error) {
	return b.newSpace(ctx, label, inputs, nil)
}

func (b *isolatingBuild) newSpace(ctx context.Context, label string, inputs, outputs []string) (StepSpace, error) {
	// The build-global counter (not the plan index) numbers the directory, so
	// uniqueness never depends on the caller passing a distinct label.
	n := b.stepCounter.Add(1)
	dir := filepath.Join(b.stepsDir, fmt.Sprintf("%02d-%s", n, sanitizeLabel(label)))

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create step workspace %q: %w", dir, err)
	}

	err = b.materializeSpace(ctx, dir, inputs, outputs)
	if err != nil {
		// Tear down whatever was already materialized under dir, so a
		// mid-loop failure doesn't leak input copies/subvolumes until the
		// whole build is torn down (the caller never receives a StepSpace to
		// Close on this path).
		removeErr := b.backend.removeTree(dir)
		if removeErr != nil {
			slog.Error("workspace.space_cleanup", "dir", dir, "error", removeErr)
		}

		return nil, err
	}

	return &isolatingSpace{backend: b.backend, artifacts: b.artifacts, dir: dir, outputs: outputs}, nil
}

// materializeSpace populates an already-created step directory with a copy or
// snapshot of each input and an empty directory for each output. On any
// error the caller (newSpace) removes dir.
func (b *isolatingBuild) materializeSpace(ctx context.Context, dir string, inputs, outputs []string) error {
	for _, in := range inputs {
		err := validateArtifactName(in)
		if err != nil {
			return err
		}

		src := filepath.Join(b.artifacts, in)

		_, statErr := os.Stat(src)
		if statErr != nil {
			return fmt.Errorf("input %q: %w", in, statErr)
		}

		err = b.backend.materialize(ctx, src, filepath.Join(dir, in))
		if err != nil {
			return fmt.Errorf("materializing input %q: %w", in, err)
		}
	}

	for _, out := range outputs {
		err := validateArtifactName(out)
		if err != nil {
			return err
		}

		err = b.backend.createEmpty(ctx, filepath.Join(dir, out))
		if err != nil {
			return fmt.Errorf("creating output %q: %w", out, err)
		}
	}

	return nil
}

// Close removes the build's root directory. b.root itself is a plain
// directory (created directly by NewBuild), but it holds the backend's
// materialized subvolumes (resource/artifact/step trees), so it goes
// through backend.removeTree — which, on btrfs, deletes those subvolumes
// explicitly instead of relying on os.RemoveAll's rmdir of a subvolume.
func (b *isolatingBuild) Close() error {
	err := b.backend.removeTree(b.root)
	if err != nil {
		return fmt.Errorf("could not remove build workspace %q: %w", b.root, err)
	}

	return nil
}

// isolatingSpace is one step's materialized directory under an isolating
// provider: an <input>/ copy or snapshot of each declared input, plus an
// empty <output>/ directory for each declared output.
type isolatingSpace struct {
	backend   treeBackend
	artifacts string
	dir       string
	outputs   []string
}

func (s *isolatingSpace) Dir() string { return s.dir }

// Capture persists each declared output back into the build's artifact
// store, replacing any artifact already there under that name (deterministic
// since steps run sequentially — there is no concurrent writer to race). A
// declared output directory that no longer exists when the step finished is
// an error: the step promised to produce it.
func (s *isolatingSpace) Capture(ctx context.Context) error {
	for _, out := range s.outputs {
		src := filepath.Join(s.dir, out)

		_, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("step declared output %q but it does not exist: %w", out, err)
		}

		dst := filepath.Join(s.artifacts, out)

		err = s.backend.remove(dst)
		if err != nil {
			return fmt.Errorf("replacing existing artifact %q: %w", out, err)
		}

		err = s.backend.materialize(ctx, src, dst)
		if err != nil {
			return fmt.Errorf("capturing output %q: %w", out, err)
		}
	}

	return nil
}

// Close removes the step's directory. s.dir is a plain directory (created
// directly by newSpace) wrapping the backend's input/output subvolumes, so
// it goes through backend.removeTree — see isolatingBuild.Close.
func (s *isolatingSpace) Close() error {
	err := s.backend.removeTree(s.dir)
	if err != nil {
		return fmt.Errorf("could not remove step workspace %q: %w", s.dir, err)
	}

	return nil
}

// newIsolatingRoot resolves the on-disk root an isolating provider
// materializes builds under: root if the config set one (created if
// missing), otherwise a fresh system-temp directory the provider owns and
// removes on Close.
func newIsolatingRoot(configuredRoot string) (root string, ownsRoot bool, err error) {
	if configuredRoot != "" {
		err = os.MkdirAll(configuredRoot, 0o750)
		if err != nil {
			return "", false, fmt.Errorf("could not create workspace root %q: %w", configuredRoot, err)
		}

		return configuredRoot, false, nil
	}

	root, err = os.MkdirTemp("", "steps-ws-*")
	if err != nil {
		return "", false, fmt.Errorf("could not create workspace root: %w", err)
	}

	return root, true, nil
}

// validateArtifactFlow statically checks that every task/agent/put step's
// declared inputs name an artifact available by that point in the plan (a
// resource an earlier get fetches, or an output an earlier task/agent
// produces). It runs no check/in/out command — unlike PlanChains, it is
// always safe to run, even under --force, which skips PlanChains entirely.
// get's version:every fans out per resolved version but never changes which
// artifact *names* exist, so a single linear walk over the plan covers
// every branch that fan-out could produce.
//
// Crucially, a get starts a *fresh* triggered build (see runTriggeredBuild)
// whose artifact store is empty except for the resource it fetches — it does
// not inherit artifacts produced before it. So a get resets the available
// set to just its own resource rather than adding to it; otherwise this
// check would pass an input referencing a pre-get artifact that the runtime
// can't actually see, exactly the late failure it exists to prevent.
func validateArtifactFlow(cfg *Config, job *Job) error {
	if cfg.Workspace == nil {
		return nil
	}

	available := map[string]bool{}

	for i, step := range job.Plan {
		err := validateStepArtifactFlow(cfg, job.Name, i, step, available)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateStepArtifactFlow checks one step's declared inputs against
// available (the artifact names produced by earlier steps in the same
// walk) and, for task/agent steps, adds this step's own declared outputs to
// available for steps after it.
func validateStepArtifactFlow(cfg *Config, jobName string, i int, step Step, available map[string]bool) error {
	switch {
	case step.Get != "":
		// A get triggers a fresh build whose store starts empty, so drop
		// everything from prior builds and keep only this get's resource.
		clear(available)
		available[step.Get] = true

		return nil
	case step.Put != "":
		return checkInputsAvailable(jobName, i, "put", step.Put, step.Inputs, available)
	case step.Task != "":
		return validateTaskArtifactFlow(cfg, jobName, i, step, available)
	case step.Agent != "":
		err := checkInputsAvailable(jobName, i, "agent", step.Agent, step.Inputs, available)
		if err != nil {
			return err
		}

		for _, out := range step.Outputs {
			available[out] = true
		}

		return nil
	default:
		return nil
	}
}

func validateTaskArtifactFlow(cfg *Config, jobName string, i int, step Step, available map[string]bool) error {
	rt, err := resolveTask(cfg, step)
	if err != nil {
		return fmt.Errorf("job %q step %d: %w", jobName, i, err)
	}

	err = checkInputsAvailable(jobName, i, "task", rt.name, rt.inputs, available)
	if err != nil {
		return err
	}

	for _, out := range rt.outputs {
		available[out] = true
	}

	return nil
}

func checkInputsAvailable(jobName string, i int, kind, name string, inputs []string, available map[string]bool) error {
	for _, in := range inputs {
		if !available[in] {
			return fmt.Errorf("job %q step %d (%s %q): input %q is not a resource fetched or an output produced earlier in the plan",
				jobName, i, kind, name, in)
		}
	}

	return nil
}

// stableStrings returns a non-nil copy of names, so json.Marshal always
// encodes it as [] rather than null — a nil inputs/outputs list and an
// explicit inputs: [] must hash identically, since they mean the same thing
// (no artifacts).
func stableStrings(names []string) []string {
	out := make([]string, len(names))
	copy(out, names)

	return out
}

var errUnsupportedPlatform = errors.New("unsupported platform")
