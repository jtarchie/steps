// Package workspace materializes the per-step/per-build filesystem views a
// job's steps run against: per-step directories, copied or btrfs-snapshotted,
// built from each step's declared inputs/outputs. It also statically validates
// a job's declared artifact flow before any step runs.
package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	Close() error
}

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

	switch ws.EffectiveStrategy() {
	case "copy":
		return newCopyProvider(ws, keep)
	case "btrfs":
		return newBtrfsProvider(ws, keep)
	default:
		return nil, fmt.Errorf("unknown workspace strategy %q", ws.Strategy)
	}
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

// Root is where this build's files are, so a failed isolated run can be
// recorded and reported. Without it the RootedBuild assertion in RunJob
// failed, StartRun was never called, and a run left no row to resume from —
// while still printing an id promising exactly that.
func (b *isolatingBuild) Root() string { return b.root }

// HasArtifact reports whether name is in the build store here, or held by a
// worker on this build's behalf.
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

// --- isolatingProvider: common lifecycle over a pluggable treeBackend ---

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

// buildDirPrefix is what every build directory under an isolating root starts
// with — the marker sweepStaleBuilds recognizes as ours to remove.
const buildDirPrefix = "b-"

// isolatingProvider is the Provider for every strategy (copy, btrfs); they
// differ only in their treeBackend.
type isolatingProvider struct {
	backend  treeBackend
	validate func() error
	root     string
	ownsRoot bool
	keep     bool
	// reuse, when set, is an existing build directory a resumed run continues
	// in instead of creating a new one (see Reuse).
	reuse string
	// token distinguishes this invocation's build directories from any other's
	// — see newInvocationToken.
	token string
	// cache is the cross-build resource cache, nil when the pipeline did not
	// opt in (see config.CacheConfig).
	cache *resourceCache
	// stepCache reuses task/agent outputs across runs, nil when the root is
	// this provider's own temp directory — see enableCache.
	stepCache *stepCache
	builds    atomic.Int64
}

// enableCache attaches the two cross-build caches.
//
// The resource cache is opt-in (cache.resources), because reusing a fetched
// version changes an observable thing: what a get: puts on disk for a version
// whose content moved underneath it.
//
// The step cache asks for no separate opt-in — a durable workspace.root: is
// the whole requirement, and configuring one is already the deliberate act.
// Both live under that root because a cache discarded at the end of the run
// that filled it is only a slower way to run, and a provider-owned temp root
// is removed at Close. Per-step control is volatile:, on the step whose result
// must never be reused.
func (p *isolatingProvider) enableCache(ws *config.WorkspaceConfig) error {
	if ws.CacheEnabled() {
		cache, err := newResourceCache(p.backend, p.root, ws.CacheMaxEntries())
		if err != nil {
			return err
		}

		p.cache = cache
	}

	if p.ownsRoot {
		return nil
	}

	stepCache, err := newStepCache(p.backend, p.root, defaultStepCacheMaxEntries)
	if err != nil {
		return err
	}

	p.stepCache = stepCache

	return nil
}

func (p *isolatingProvider) Validate() error {
	err := p.probe()
	if err != nil {
		return err
	}

	p.sweepStaleBuilds()

	return nil
}

func (p *isolatingProvider) probe() error {
	if p.validate == nil {
		return nil
	}

	return p.validate()
}

// Probe is Validate without the stale-build sweep, for a provider opened on a root this process may already be building under: the sweep takes every build directory there for a crashed process's leftovers.
func Probe(p Provider) error {
	isolating, ok := p.(*isolatingProvider)
	if !ok {
		// Nothing else sweeps, so its Validate is already only a probe.
		return p.Validate() //nolint:wrapcheck // the provider's own error names the root and the rule it broke
	}

	return isolating.probe()
}

// sweepStaleBuilds removes build directories left behind by an earlier run.
//
// A build is normally torn down at Close, so anything still here belongs to a
// process that never got to run it: a SIGKILL, a panicking host, a pulled
// plug. Under strategy: btrfs those directories hold live subvolumes, which
// ordinary cleanup never reclaims and which need `btrfs subvolume delete` —
// so without this they accumulate, and the disk they hold is only recoverable
// by hand.
//
// Skipped entirely under --keep-workspace: that flag means "leave the files
// for me to look at", and deleting the previous run's kept workspace at the
// start of the next one would defeat the only reason to pass it.
//
// Best-effort and non-fatal: a failure here is logged, never returned. Not
// being able to tidy up is not a reason to refuse to run.
func (p *isolatingProvider) sweepStaleBuilds() {
	if p.keep {
		return
	}

	entries, err := os.ReadDir(p.root)
	if err != nil {
		slog.Debug("workspace.sweep_skipped", "root", p.root, "error", err)

		return
	}

	for _, entry := range entries {
		// Only build directories. The cache lives under this root too and is
		// the whole point of surviving a crash, so the prefix test is what
		// keeps the sweep from throwing away the asset along with the garbage.
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), buildDirPrefix) {
			continue
		}

		dir := filepath.Join(p.root, entry.Name())

		slog.Info("workspace.sweep_stale_build", "dir", dir)

		removeErr := p.backend.removeTree(dir)
		if removeErr != nil {
			slog.Warn("workspace.sweep_failed", "dir", dir, "error", removeErr)
		}
	}
}

// Reuse continues a previous run in an existing build tree.
//
// Every NewBuild answers with that tree: a resumed run is one run continuing,
// and splitting it back into fresh per-build directories would hide the
// artifacts it is resuming FOR.
func (p *isolatingProvider) Reuse(dir string) { p.reuse = dir }

// retainedBuild names a build directory from THIS invocation that is still on
// disk, or "" when every build was closed. Scoped to the invocation's own
// token so another process's builds under a shared root are never mistaken for
// ours.
func (p *isolatingProvider) retainedBuild() string {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return ""
	}

	prefix := buildDirPrefix + p.token + "-"

	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) {
			return filepath.Join(p.root, entry.Name())
		}
	}

	return ""
}

func (p *isolatingProvider) NewBuild(ctx context.Context, label string) (BuildWorkspace, error) {
	if p.reuse != "" {
		// The tree already has its artifacts/ and steps/ from the run being
		// continued; per-step directories under steps/ are rebuilt per step
		// anyway, which is what makes an isolating strategy resumable at all.
		slog.DebugContext(ctx, "workspace.reuse", "dir", p.reuse, "backend", "isolating")

		return &isolatingBuild{
			backend: p.backend, root: p.reuse,
			artifacts: filepath.Join(p.reuse, "artifacts"),
			stepsDir:  filepath.Join(p.reuse, "steps"),
			keep:      true, // never tear down a tree we did not create
			cache:     p.cache,
			stepCache: p.stepCache,
		}, nil
	}

	// The token comes before the counter because the counter alone is not
	// unique: it restarts at 1 in every process, so a crashed run's leftover
	// b-1-<label> would collide with the next run's — MkdirAll succeeds on the
	// existing directory and the backend then fails creating an artifact
	// subvolume that is already there. sweepStaleBuilds normally removes those
	// first; the token is what makes the collision impossible even when the
	// sweep is skipped (--keep-workspace) or two processes share a root.
	n := p.builds.Add(1)
	root := filepath.Join(p.root, fmt.Sprintf("%s%s-%d-%s", buildDirPrefix, p.token, n, sanitizeLabel(label)))

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

	slog.DebugContext(ctx, "workspace.create", "dir", root, "backend", "isolating")

	return &isolatingBuild{
		backend: p.backend, root: root, artifacts: artifacts, stepsDir: steps,
		keep: p.keep, cache: p.cache, stepCache: p.stepCache,
	}, nil
}

func (p *isolatingProvider) Close() error {
	if !p.ownsRoot {
		return nil
	}

	if keepWorkspace(p.keep, p.root) {
		return nil
	}

	// A build that was never closed was kept ON PURPOSE — the pipeline skips
	// CloseBuild when a build failed, so its tree is what --resume continues
	// in. Removing the root here would delete it, which is what made an
	// isolated run unresumable no matter what the provider allowed: the run
	// row survived, pointing at a directory this line had just erased.
	if p.retainedBuild() != "" {
		slog.Info("workspace.kept_for_resume", "dir", p.root)

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
	keep        bool
	cache       *resourceCache
	stepCache   *stepCache
	stepCounter atomic.Int64

	// digests memoizes artifactDigest per artifact name. An artifact's bytes
	// change only when something in this file replaces the directory (a get
	// fetching, a step capturing, a matrix resetting, a cache hit restoring),
	// and each of those forgets the entry — so the memo is exact rather than
	// merely close, and hashing a large checkout costs the same whether one
	// step reads it or ten do.
	digestMu    sync.Mutex
	digests     map[string]string
	generations map[string]uint64

	// remote is every artifact a worker holds on this build's behalf, by
	// name — see remote.go.
	remoteMu sync.Mutex
	remote   map[string]RemoteArtifact
	pulling  map[string]chan struct{}
}

// artifactDigest is the content hash of one artifact in the build store, for
// the step cache's action key.
//
// An artifact that does not exist digests as the empty tree rather than
// failing: a step may legitimately declare an input no earlier step produced
// (materializeSpace would fail later, and with a better message), and a cache
// lookup is not the place to decide that.
func (b *isolatingBuild) artifactDigest(name string) (string, error) {
	digest, generation, ok := b.rememberedDigest(name)
	if ok {
		return digest, nil
	}

	// A tree a worker holds is identified by the digest it is filed under
	// rather than pulled to be walked: a cache key is a question about
	// identity, not a reader of the bytes, and the two names for one tree
	// cost at most a miss when the same input is met local one build and
	// remote the next.
	if remote, held := b.remoteArtifact(name); held {
		return remote.Digest, nil
	}

	// Outside the lock: walking a large checkout is the expensive part, and
	// holding a build-wide mutex across it would serialize every concurrent
	// branch behind one tree walk. Two branches racing the same artifact each
	// compute it and agree, which costs one redundant walk and no correctness.
	digest, err := digestTree(filepath.Join(b.artifacts, name))
	if err != nil {
		// A step may legitimately declare an input no earlier step produced;
		// materializeSpace fails on that later, with a better message than a
		// cache lookup could give.
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}

		digest = ""
	}

	b.rememberDigest(name, digest, generation)

	return digest, nil
}

// rememberedDigest reports a memoized digest, and the generation the artifact
// was at when it was read — what tells rememberDigest whether the bytes moved
// while it was being walked.
func (b *isolatingBuild) rememberedDigest(name string) (digest string, generation uint64, ok bool) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	digest, ok = b.digests[name]

	return digest, b.generations[name], ok
}

// rememberDigest memoizes a freshly computed digest, unless the artifact was
// replaced while it was being computed — in which case the digest describes
// bytes that are already gone, and memoizing it would hand the next step a key
// for content nobody can read any more.
func (b *isolatingBuild) rememberDigest(name, digest string, generation uint64) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	if b.generations[name] != generation {
		return
	}

	if b.digests == nil {
		b.digests = map[string]string{}
	}

	b.digests[name] = digest
}

// superseded is what a capture or a fresh fetch says about an artifact: the
// bytes are HERE now, whatever a worker still holds and whatever digest was
// remembered. Both records or neither — a stale hold pulls the old tree over
// a new capture on the next read, which is data loss with nothing red.
func (b *isolatingBuild) superseded(names []string) {
	b.forgetRemote(names)
	b.forgetDigests(names)
}

// forgetDigests drops memoized digests for artifacts whose bytes just changed,
// and bumps their generation so a walk already in flight over the old content
// cannot install its answer afterwards.
func (b *isolatingBuild) forgetDigests(names []string) {
	b.digestMu.Lock()
	defer b.digestMu.Unlock()

	if b.generations == nil {
		b.generations = map[string]uint64{}
	}

	for _, name := range names {
		delete(b.digests, name)
		b.generations[name]++
	}
}

// FetchResource implements CachingBuild. With no cache configured it is
// exactly ResourceDir followed by fetch — the behavior every pipeline had
// before the cache existed.
func (b *isolatingBuild) FetchResource(ctx context.Context, name, cacheKey string, fetch func(dir string) error) (string, error) {
	dir, err := b.ResourceDir(ctx, name)
	if err != nil {
		return "", err
	}

	// After the fetch on BOTH paths, not only the cached one: ResourceDir's own
	// forget happens before these bytes exist, so without this the digest
	// memoized for an artifact could be the empty tree it was created as.
	defer b.forgetDigests([]string{name})

	if b.cache == nil || cacheKey == "" {
		return dir, fetch(dir)
	}

	// The cache lives on this disk, so it is a local reader: a tree a placed
	// in: left on its worker comes home to be filed.
	return dir, b.cache.Fetch(ctx, cacheKey, dir, func() error {
		err := fetch(dir)
		if err != nil {
			return err
		}

		return b.ensureLocal(ctx, name)
	})
}

func (b *isolatingBuild) ResourceDir(ctx context.Context, name string) (string, error) {
	err := config.ValidateArtifactName(name)
	if err != nil {
		return "", fmt.Errorf("resource %q: %w", name, err)
	}

	dir := filepath.Join(b.artifacts, name)

	err = b.backend.createEmpty(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("could not create resource dir %q: %w", dir, err)
	}

	b.superseded([]string{name})

	return dir, nil
}

func (b *isolatingBuild) TaskSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error) {
	return b.newSpace(ctx, label, inputs, outputs, inputMapping, outputMapping)
}

func (b *isolatingBuild) GuardSpace(ctx context.Context, label string, inputs []string, inputMapping map[string]string, all bool) (StepSpace, error) {
	if all {
		names, err := b.allArtifacts()
		if err != nil {
			return nil, err
		}

		inputs, inputMapping = names, nil
	}

	present := make([]string, 0, len(inputs))

	for _, in := range inputs {
		// The MAPPED name is what the store holds; the declared name is only
		// what the directory is called inside the space. Checking the declared
		// one would read every mapped input as unproduced, which is the guard
		// answering false about an artifact that is right there.
		artifact := mappedName(in, inputMapping)

		// A name newSpace would reject is kept rather than skipped, so
		// leniency covers exactly "not produced" and the error still comes
		// from the one place that phrases it well.
		if config.ValidateArtifactName(in) != nil || config.ValidateArtifactName(artifact) != nil {
			present = append(present, in)

			continue
		}

		_, err := os.Lstat(filepath.Join(b.artifacts, artifact))
		if os.IsNotExist(err) {
			if _, held := b.remoteArtifact(artifact); !held {
				continue
			}
		}

		// Anything else — an unreadable directory, a symlinked artifact — is
		// newSpace's to report too.
		present = append(present, in)
	}

	return b.newSpace(ctx, label, present, nil, inputMapping, nil)
}

func (b *isolatingBuild) PutSpace(ctx context.Context, label string, inputs []string, all bool) (StepSpace, error) {
	if all {
		names, err := b.allArtifacts()
		if err != nil {
			return nil, err
		}

		inputs = names
	}

	return b.newSpace(ctx, label, inputs, nil, nil, nil)
}

// allArtifacts lists every artifact name currently in the build store, for a
// put step's inputs: all. Order is sorted so the materialized view is stable.
func (b *isolatingBuild) allArtifacts() ([]string, error) {
	entries, err := os.ReadDir(b.artifacts)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has been produced yet
		}

		return nil, fmt.Errorf("could not list build artifacts: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.Contains(entry.Name(), stagedMarker) {
			names = append(names, entry.Name())
		}
	}

	for _, name := range b.remoteNames() {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}

	slices.Sort(names)

	return names, nil
}

func (b *isolatingBuild) newSpace(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string) (StepSpace, error) {
	return b.newSpaceLeaving(ctx, label, inputs, outputs, inputMapping, outputMapping, nil)
}

// newSpaceLeaving is newSpace with some inputs left on the workers that hold
// them (PlacedSpaces): those are neither pulled nor materialized, and an
// output of the same name is not created empty over the tree the worker
// will place.
func (b *isolatingBuild) newSpaceLeaving(ctx context.Context, label string, inputs, outputs []string, inputMapping, outputMapping map[string]string, leaving map[string]RemoteArtifact) (StepSpace, error) {
	// The build-global counter (not the plan index) numbers the directory, so
	// uniqueness never depends on the caller passing a distinct label.
	n := b.stepCounter.Add(1)
	dir := filepath.Join(b.stepsDir, fmt.Sprintf("%02d-%s", n, sanitizeLabel(label)))

	err := os.MkdirAll(dir, 0o750)
	if err != nil {
		return nil, fmt.Errorf("could not create step workspace %q: %w", dir, err)
	}

	err = b.materializeSpace(ctx, dir, inputs, outputs, inputMapping, outputMapping, leaving)
	if err != nil {
		// Tear down whatever was already materialized under dir, so a
		// mid-loop failure doesn't leak input copies/subvolumes until the
		// whole build is torn down (the caller never receives a StepSpace to
		// Close on this path).
		removeErr := b.backend.removeTree(dir)
		if removeErr != nil {
			slog.ErrorContext(ctx, "workspace.space_cleanup", "dir", dir, "error", removeErr)
		}

		return nil, err
	}

	return &isolatingSpace{
		backend: b.backend, artifacts: b.artifacts, dir: dir,
		outputs: outputs, outputMapping: outputMapping, keep: b.keep,
		captured: b.superseded, left: leaving,
	}, nil
}

// materializeSpace populates an already-created step directory with a copy or
// snapshot of each input under its declared name and an empty directory for
// each output — except an output that is also an input, which keeps the
// materialized content (read-modify-write; see validateArtifactNames). inputMapping/outputMapping rename a declared name to the plan-
// artifact name it draws from / captures to: the directory on disk keeps the
// declared name (what the task's run: expects), while the artifact copied in /
// captured out uses the mapped name. On any error the caller (newSpace)
// removes dir.
func (b *isolatingBuild) materializeSpace(ctx context.Context, dir string, inputs, outputs []string, inputMapping, outputMapping map[string]string, leaving map[string]RemoteArtifact) error {
	for _, in := range inputs {
		err := config.ValidateArtifactName(in)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		if _, left := leaving[in]; left {
			continue
		}

		artifact := mappedName(in, inputMapping)

		err = b.ensureLocal(ctx, artifact)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		src := filepath.Join(b.artifacts, artifact)

		err = rejectSymlinkSrc(src)
		if err != nil {
			return fmt.Errorf("input %q: %w", in, err)
		}

		err = b.backend.materialize(ctx, src, filepath.Join(dir, in))
		if err != nil {
			return fmt.Errorf("materializing input %q: %w", in, err)
		}
	}

	return b.materializeOutputs(ctx, dir, inputs, outputs, outputMapping)
}

// materializeOutputs is materializeSpace's output half, split out to stay
// within the linter's complexity budget.
func (b *isolatingBuild) materializeOutputs(ctx context.Context, dir string, inputs, outputs []string, outputMapping map[string]string) error {
	materialized := make(map[string]bool, len(inputs))
	for _, in := range inputs {
		materialized[in] = true
	}

	for _, out := range outputs {
		err := validateOutputName(out, outputMapping)
		if err != nil {
			return err
		}

		// An output that is also an input is read-modify-write: the input
		// materialization already populated the directory with the artifact's
		// current content, and Capture writes it back. Creating an empty
		// directory here would clobber exactly the state the step declared
		// it wants to carry forward.
		if materialized[out] {
			continue
		}

		err = b.backend.createEmpty(ctx, filepath.Join(dir, out))
		if err != nil {
			return fmt.Errorf("creating output %q: %w", out, err)
		}
	}

	return nil
}

// validateOutputName holds a declared output, and where outputMapping
// captures it, to the artifact naming rules.
//
// A mapped output can't smuggle a bad destination into the store on Capture.
// The PATH rule rather than the plain name rule, because a collecting matrix's
// cells legitimately capture under coordinates (findings/alpha/fast) —
// user-written mapping values are pinned to the strict name rule at load, so
// only machine-composed paths carry slashes here.
func validateOutputName(out string, outputMapping map[string]string) error {
	err := config.ValidateArtifactName(out)
	if err != nil {
		return fmt.Errorf("output %q: %w", out, err)
	}

	if mapped := mappedName(out, outputMapping); mapped != out {
		err = config.ValidateArtifactPath(mapped)
		if err != nil {
			return fmt.Errorf("output %q (mapped to %q): %w", out, mapped, err)
		}
	}

	return nil
}

// ArtifactResetter is the optional BuildWorkspace capability a collecting
// across: block uses (see internal/pipeline): ResetArtifact replaces whatever
// the store holds under name with a fresh empty directory, before any cell
// captures. That is what (a) restores the "a later producer replaces an
// earlier one's artifact" sequential contract that per-coordinate capture
// alone cannot — each cell's Capture replaces only its own subdirectory — and
// (b) guarantees the collected artifact EXISTS even when every cell failed or
// the matrix expanded to zero cells, so the consumer walks what survived
// (possibly nothing) instead of failing to materialize a missing input.
//
// Optional rather than on BuildWorkspace; every provider's build implements
// it, so a build that does not is a wiring bug the caller reports.
type ArtifactResetter interface {
	ResetArtifact(ctx context.Context, name string) error
}

// ResetArtifact implements ArtifactResetter.
func (b *isolatingBuild) ResetArtifact(ctx context.Context, name string) error {
	err := config.ValidateArtifactName(name)
	if err != nil {
		return fmt.Errorf("reset artifact: %w", err)
	}

	dst := filepath.Join(b.artifacts, name)

	err = b.backend.remove(dst)
	if err != nil {
		return fmt.Errorf("resetting artifact %q: %w", name, err)
	}

	err = b.backend.createEmpty(ctx, dst)
	if err != nil {
		return fmt.Errorf("resetting artifact %q: %w", name, err)
	}

	b.forgetRemote([]string{name})
	b.forgetDigests([]string{name})

	return nil
}

// mappedName renames name through mapping (declared name -> plan-artifact
// name), returning name unchanged when unmapped.
func mappedName(name string, mapping map[string]string) string {
	if mapped, ok := mapping[name]; ok {
		return mapped
	}

	return name
}

// Close removes the build's root directory. b.root itself is a plain
// directory (created directly by NewBuild), but it holds the backend's
// materialized subvolumes (resource/artifact/step trees), so it goes
// through backend.removeTree — which, on btrfs, deletes those subvolumes
// explicitly instead of relying on os.RemoveAll's rmdir of a subvolume.
func (b *isolatingBuild) Close() error {
	// The provider prints its own kept-root line when it owns the root; a
	// build under a user-supplied workspace.root: has none, so say it here.
	if b.keep {
		slog.Debug("workspace.kept", "dir", b.root)

		return nil
	}

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
	backend       treeBackend
	artifacts     string
	dir           string
	outputs       []string
	outputMapping map[string]string
	// keep mirrors the build's --keep-workspace: the step directory is left
	// on disk at Close, so the files a failed step had just written — the
	// first thing anyone wants to look at — survive to be read.
	keep bool
	// captured tells the build which artifacts this step just replaced, so a
	// digest remembered for one of them is not served to the next step's cache
	// key after the bytes behind it changed.
	captured func(names []string)
	// left are the inputs this space left on their holders (PlacedSpaces). One
	// that is also an output has no directory here when the worker kept the
	// result: the record of the hold IS the capture.
	left map[string]RemoteArtifact
}

func (s *isolatingSpace) Dir() string { return s.dir }

// Kept implements the optional capability workspace.Kept reads.
func (s *isolatingSpace) Kept() bool { return s.keep }

// Capture persists each declared output back into the build's artifact
// store, replacing whatever is already at its DESTINATION — the plain
// artifact name for an ordinary step, a per-cell coordinate subdirectory
// (findings/alpha) for a cell of a collecting matrix. Ordinary steps run
// sequentially, so a plain-name capture has no concurrent writer to race;
// collecting cells under max_in_flight: capture concurrently, and are safe
// only because CollectedOutputMapping gives every cell a disjoint
// destination and the block reset (ArtifactResetter) has already created
// the shared parent. A collecting cell therefore replaces only its own
// subdirectory — whole-artifact replacement for the block is the reset's
// job, before any cell runs. A
// declared output directory that no longer exists when the step finished is
// an error: the step promised to produce it. rejectSymlinkSrc additionally
// refuses a declared output that is itself a symlink: the step's own run:/
// tool commands could otherwise delete the materialized output directory
// and replace it with a symlink to an arbitrary host path (e.g. /etc, a
// home directory), which the copy backend's `cp ... src/. dst` would then
// silently dereference, exfiltrating the real target's contents into the
// pipeline's artifact store as if it were the legitimate output.
func (s *isolatingSpace) Capture(ctx context.Context) error {
	if s.captured != nil {
		// Before the loop, not after: a capture that fails partway has still
		// replaced the artifacts it got to, and those digests are stale
		// whether or not the step as a whole succeeded.
		defer s.captured(artifactNames(s.outputs, s.outputMapping))
	}

	for _, out := range s.outputs {
		src := filepath.Join(s.dir, out)

		if _, left := s.left[out]; left {
			_, statErr := os.Lstat(src)
			if errors.Is(statErr, fs.ErrNotExist) {
				continue
			}
		}

		err := rejectSymlinkSrc(src)
		if err != nil {
			return fmt.Errorf("step declared output %q: %w", out, err)
		}

		// The directory on disk carries the declared name; output_mapping
		// captures it back into the store under the plan-artifact name.
		artifact := mappedName(out, s.outputMapping)
		dst := filepath.Join(s.artifacts, artifact)

		// Staged, so an interrupted capture leaves the previous artifact for a
		// --resume rather than an empty one.
		err = replaceTree(dst, s.backend.remove, func(staged string) error {
			return s.backend.materialize(ctx, src, staged)
		})
		if err != nil {
			return fmt.Errorf("capturing output %q: %w", out, err)
		}
	}

	return nil
}

// Close removes the step's directory. s.dir is a plain directory (created
// directly by newSpace) wrapping the backend's input/output subvolumes, so
// it goes through backend.removeTree — see isolatingBuild.Close. Under
// --keep-workspace the directory stays, like everything else in the tree.
func (s *isolatingSpace) Close() error {
	if s.keep {
		return nil
	}

	err := s.backend.removeTree(s.dir)
	if err != nil {
		return fmt.Errorf("could not remove step workspace %q: %w", s.dir, err)
	}

	return nil
}

// newInvocationToken returns a short random string identifying one steps
// invocation's build directories.
//
// Random rather than the pid: pids are reused, and a run whose leftovers are
// still on disk is exactly the run whose pid is now free. It only has to be
// distinct from whatever else is under the root, so eight hex characters is
// ample; a failure to read randomness falls back to the process start time,
// since a token that is merely probably-unique still beats no token.
func newInvocationToken() string {
	var buf [4]byte

	_, err := rand.Read(buf[:])
	if err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}

	return hex.EncodeToString(buf[:])
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

var errUnsupportedPlatform = errors.New("unsupported platform")

// Resumable is a Provider that can continue a previous run's workspace.
//
// It continues in the BUILD tree: what a provider tears down per step is the
// step's own view, which a resume does not need — the artifacts finished steps
// produced live at the build root, and per-step views are materialized from
// them every time a step runs, resumed or not.
type Resumable interface {
	Reuse(dir string)
	Provider
}

// RootedBuild is a BuildWorkspace that can say where its files are, so a
// failed run can print the directory a resume will continue in.
type RootedBuild interface {
	Root() string
}

// ArtifactChecker is a BuildWorkspace that can say whether it holds an
// artifact, so a resume that keeps a get's artifact rather than fetching it
// again can refuse a tree that lost it instead of continuing without it.
type ArtifactChecker interface {
	HasArtifact(name string) bool
}

// CachingBuild is a BuildWorkspace that can reuse a resource version fetched
// by an earlier build instead of fetching it again.
//
// An optional interface, like Resumable and RootedBuild, rather than a method
// on BuildWorkspace: callers that do not find it simply fetch, which is what
// every pipeline did before the cache existed. A build with no cache
// configured serves it as exactly that — see isolatingBuild.FetchResource.
type CachingBuild interface {
	// FetchResource returns the directory for resource name, populated either
	// from the cache entry for cacheKey or by calling fetch. fetch receives the
	// directory to populate and is called at most once; when it is not called,
	// the directory already holds that version's content.
	//
	// A cache failure is never a fetch failure: anything that goes wrong with
	// the cache falls back to calling fetch.
	FetchResource(ctx context.Context, name, cacheKey string, fetch func(dir string) error) (string, error)
}
