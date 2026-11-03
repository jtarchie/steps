// Package workspace materializes the per-step/per-build filesystem views a
// job's steps run against: per-step directories, copied (copy-on-write where the filesystem offers it),
// built from each step's declared inputs/outputs. It also statically validates
// a job's declared artifact flow before any step runs.
package workspace

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"

	"github.com/jtarchie/steps/internal/config"
)

// Provider is built once per CLI invocation from cfg.Workspace (nil taking
// every default — see NewProvider). It owns whatever on-disk root its strategy
// needs and fails fast, at startup, on anything that would only surface as a
// confusing mid-build error otherwise (wrong platform, wrong filesystem,
// missing binaries).
type Provider interface {
	Validate() error
	// NewBuild creates the workspace for one triggered build (see
	// runTriggeredBuild) — or, called once from RunJob, the job-level build
	// that steps preceding any get run in. Each call is fully independent:
	// it never shares state with a build from a previous call, matching
	// today's per-triggered-build isolation (see runTriggeredBuild's doc).
	NewBuild(ctx context.Context, label string) (BuildWorkspace, error)
	// Probe is Validate without the stale-build sweep, for a provider opened
	// on a root this process may already be building under: the sweep takes
	// every build directory there for a crashed process's leftovers.
	Probe() error
	// Reuse continues a previous run in an existing build tree, or with ""
	// stops doing so. It continues in the BUILD tree: what a provider tears
	// down per step is the step's own view, which a resume does not need —
	// the artifacts finished steps produced live at the build root, and
	// per-step views are materialized from them every time a step runs.
	Reuse(dir string)
	// Resuming is Reuse for one run: a provider whose every NewBuild answers with dir, leaving this one as it was, since a daemon builds other runs beside the one it resumes.
	Resuming(dir string) Provider
	// Spare keeps dirs out of the stale-build sweep, the trees of runs a restart resumes. Called before Validate, which sweeps.
	Spare(dirs []string)
	// AttachArtifactStore wires the two halves of the artifact store into the
	// step cache, reporting whether anything was attached — false with no
	// durable workspace.root:, where there is no local cache to mirror.
	AttachArtifactStore(blobs BlobStore, index StepIndex) bool
	Close() error
}

// Every capability is on the interface rather than type-asserted at its call
// site, so a wrapper that embeds one forwards all of it instead of silently
// switching caching or placement off.
var (
	_ Provider       = (*isolatingProvider)(nil)
	_ BuildWorkspace = (*isolatingBuild)(nil)
)

// BuildWorkspace is one triggered build's filesystem view: where get steps
// land pristine resource contents, and where task/agent/put steps draw
// their per-step workspace from.
type BuildWorkspace interface {
	// ResourceDir returns the (empty, freshly created) directory a get step
	// should fetch resource name into.
	ResourceDir(ctx context.Context, name string) (string, error)
	// TaskSpace materializes a task or agent step's working directory: a
	// directory containing only an <input>/ copy or snapshot of each named
	// input plus an empty <output>/ directory for each named output. inputMapping/
	// outputMapping rename a declared name to the plan-artifact name it draws
	// from / is captured as (see config.Step.InputMapping); nil leaves names
	// unmapped. Agent steps carry no author-written mapping, but a cell of a
	// collecting matrix (task or agent) passes the machine-composed
	// coordinate mapping config.CollectedOutputMapping builds.
	TaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error)
	// PutSpace composes a put step's read view from its declared inputs, or —
	// when all is true (inputs: all) — from every artifact in the build store.
	// Unlike TaskSpace/agent steps, a put step never has outputs of its own.
	PutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, error)
	// GuardSpace composes a when: guard's read view: the same inputs the
	// step's own TaskSpace/PutSpace would materialize — under the same
	// inputMapping, and every artifact in the store when all is true — minus
	// any the build store does not hold.
	//
	// A guard decides whether its step runs at all, and the question it is
	// usually asked — is there anything in this artifact worth publishing —
	// is about an artifact an earlier guarded step may legitimately never
	// have written. Demanding every input first made such a guard FAIL
	// instead of answer: the missing directory errored before the command
	// that would have said "nothing to do" ever ran. Absent is an answer
	// here, not an error.
	//
	// Only "not produced" is lenient. A name the step's own space would
	// reject is kept, so it is reported from the one place that reports it;
	// and the step's own space still requires every declared input, so a
	// misspelled name is still an error the moment the step itself runs.
	GuardSpace(ctx context.Context, label string, inputs []string, inputMapping map[string]string, all bool) (StepSpace, error)
	// PlacedTaskSpace is TaskSpace for a step that runs on a worker, leaving
	// inputs other workers hold on their holders, reported by declared name.
	PlacedTaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, map[string]RemoteArtifact, error)
	// PlacedPutSpace is PutSpace, the same way.
	PlacedPutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, map[string]RemoteArtifact, error)
	// HoldRemote records an output a placed step left on its worker.
	HoldRemote(name string, remote RemoteArtifact) error
	// FetchResource returns the directory for resource name, populated either
	// from the cache entry for cacheKey or by calling fetch. fetch receives the
	// directory to populate and is called at most once; when it is not called,
	// the directory already holds that version's content. With no cache
	// configured, or an empty cacheKey, it is ResourceDir followed by fetch.
	//
	// A cache failure is never a fetch failure: anything that goes wrong with
	// the cache falls back to calling fetch.
	FetchResource(ctx context.Context, name, cacheKey string, fetch func(dir string) error) (string, error)
	// RestoreStep computes a step's action key and, when an entry for it
	// holds every declared output, materializes them into the artifact store.
	// With no durable workspace.root: it answers "miss, no key" — there is
	// nowhere for an entry to outlive the run that wrote it.
	RestoreStep(ctx context.Context, req StepCacheRequest) (StepCacheResult, error)
	// StoreStep files the outputs a step just captured under key. Called only
	// after the step succeeded and space.Capture put its outputs in the store.
	StoreStep(ctx context.Context, key string, req StepCacheRequest) error
	// ResetArtifact replaces whatever the store holds under name with a fresh
	// empty directory, for a collecting across: (see isolatingBuild's).
	ResetArtifact(ctx context.Context, name string) error
	// AttachCaches puts each cache (path relative to dir → TaskCacheKey) in place before a step runs; the func it returns files them back and must run before the space closes.
	AttachCaches(ctx context.Context, dir string, caches map[string]string) func()
	// HasArtifact reports whether name is in the build store, or held by a
	// worker on the build's behalf, so a resume that keeps a get's artifact
	// can refuse a tree that lost it.
	HasArtifact(name string) bool
	// Root is where the build's files are: recorded as the run's workspace,
	// and printed for a resume to continue in.
	Root() string
	Close() error
}

// StepSpace is one step's materialized working directory.
type StepSpace interface {
	Dir() string
	// Capture persists the step's declared outputs back into the build's
	// artifact store, so later steps can name them as inputs. Called only
	// after the step itself succeeded.
	Capture(ctx context.Context) error
	// Close removes anything TaskSpace/PutSpace/GuardSpace created for this
	// step.
	Close() error
}

// Kept reports whether space survives its own Close (--keep-workspace), so a
// caller that puts scratch state inside the step's directory can leave it in
// place for the same postmortem the directory itself is being kept for — see
// internal/agent's spill directory. False for a space that does not implement
// the optional capability.
func Kept(space StepSpace) bool {
	kept, ok := space.(interface{ Kept() bool })

	return ok && kept.Kept()
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

// NewProvider builds the provider a pipeline's workspace: block selects.
// Every provider isolates: a step's directory is materialized from its own
// declared inputs:/outputs: and nothing else. ws == nil (no block present)
// takes every default — strategy copy under a system temp root. (The old
// shared single-mutable-directory mode, where every step saw everything, is
// gone: an artifact a step never declared reaching it anyway was ambient
// data flow, and every flow here is opt-in.)
//
// keep leaves each build's directory on disk instead of removing it at Close,
// and prints where it is. A build workspace is otherwise destroyed
// unconditionally, including on the failure path — so the files an agent or
// task had just edited when it failed, the most useful thing to look at, were
// always already gone by the time the error reached the terminal.
func NewProvider(ws *config.WorkspaceConfig, keep bool) (Provider, error) {
	if ws == nil {
		ws = &config.WorkspaceConfig{}
	}

	return newCopyProvider(ws, keep)
}

// keepWorkspace reports that root is being left in place, and reports whether
// the caller should skip removing it.
func keepWorkspace(keep bool, root string) bool {
	if !keep {
		return false
	}

	slog.Info("workspace.kept", "dir", root)

	return true
}

// CloseBuild is a best-effort cleanup helper for the pipeline/agent
// packages' deferred Close calls: a cleanup failure must never mask the
// original error already being returned, so it's only logged.
func CloseBuild(bw BuildWorkspace, label string) {
	err := bw.Close()
	if err != nil {
		slog.Error("workspace.build_close", "label", label, "error", err)
	}
}

// CloseSpace is CloseBuild's StepSpace counterpart.
func CloseSpace(space StepSpace, label string) {
	err := space.Close()
	if err != nil {
		slog.Error("workspace.space_close", "label", label, "error", err)
	}
}

func (b *isolatingBuild) Root() string { return b.root }

func (b *isolatingBuild) HasArtifact(name string) bool {
	if config.ValidateArtifactName(name) != nil {
		return false
	}

	if _, held := b.remoteArtifact(name); held {
		return true
	}

	_, err := os.Lstat(filepath.Join(b.artifacts, name))

	return err == nil
}

// rejectSymlinkSrc enforces treeBackend.materialize's implicit precondition
// that src is a real directory, not a symlink. This matters specifically
// because the copy backend's `cp -R -P -p src/. dst` (and its Linux/other
// variants) dereferences a symlink AT src itself despite -P: the trailing
// "/." (needed to copy src's *contents* into an already-existing dst rather
// than creating dst as a copy of src) forces path resolution through the
// link before -P's never-follow guarantee can apply to it. -P still protects
// symlinks nested *inside* src's tree — only the top-level src argument is
// at risk, which is exactly the case a step (or an attacker-influenced task/
// agent) can trigger by deleting its materialized directory and replacing it
// with a symlink to an arbitrary host path. Checked with os.Lstat (which,
// unlike os.Stat, reports the link itself rather than resolving it) so a
// legitimate missing path still fails with the same "does not exist" shape
// callers already handle.
func rejectSymlinkSrc(src string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink, not a directory — refusing to copy/snapshot through it", src)
	}

	return nil
}

// treeBackend is how a directory is created empty, how one directory's
// contents are materialized into a fresh one, and how a materialized tree is
// torn down. copyBackend is the one implementation; tests wrap it to inject
// an interrupted copy.
type treeBackend interface {
	// createEmpty creates a fresh, empty directory at dir. dir's parent must
	// already exist.
	createEmpty(ctx context.Context, dir string) error
	// materialize copies or snapshots src's contents into a fresh directory
	// at dst (dst must not already exist; dst's parent must).
	materialize(ctx context.Context, src, dst string) error
	// remove tears down a single tree created by createEmpty/materialize.
	// It must not depend on ctx staying valid —
	// callers may invoke it during cleanup after ctx has already been
	// canceled (e.g. on SIGINT), so implementations that shell out use their
	// own bounded-lifetime context internally. remove is a no-op, not an
	// error, if dir does not exist.
	remove(dir string) error
}

// mappedName renames name through mapping (declared name -> plan-artifact
// name), returning name unchanged when unmapped.
func mappedName(name string, mapping map[string]string) string {
	if mapped, ok := mapping[name]; ok {
		return mapped
	}

	return name
}
