package workspace

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// --- isolatingProvider: common lifecycle over a treeBackend ---

// buildDirPrefix is what every build directory under an isolating root starts
// with — the marker sweepStaleBuilds recognizes as ours to remove.
const buildDirPrefix = "b-"

// isolatingProvider is the Provider, over a treeBackend.
type isolatingProvider struct {
	backend  treeBackend
	validate func() error
	root     string
	ownsRoot bool
	keep     bool
	// reuse, when set, is an existing build directory a resumed run continues
	// in instead of creating a new one (see Reuse).
	reuse string
	spare map[string]bool
	// token distinguishes this invocation's build directories from any other's
	// — see newInvocationToken.
	token string
	// cache is the cross-build resource cache, nil when the pipeline did not
	// opt in (see config.CacheConfig).
	cache *resourceCache
	// stepCache reuses task/agent outputs across runs, nil when the root is
	// this provider's own temp directory — see enableCache.
	stepCache *stepCache
	// taskCaches holds caches: directories between runs; under a provider-owned temp root they last only the run.
	taskCaches *entryStore
	builds     atomic.Int64
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

	taskCaches, err := newEntryStore(p.root, taskCacheDirName, defaultTaskCacheMaxEntries, p.backend.remove)
	if err != nil {
		return err
	}

	p.taskCaches = taskCaches

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
	err := p.Probe()
	if err != nil {
		return err
	}

	p.sweepStaleBuilds()

	return nil
}

// Probe is Validate without the stale-build sweep.
func (p *isolatingProvider) Probe() error {
	if p.validate == nil {
		return nil
	}

	return p.validate()
}

// sweepStaleBuilds removes build directories left behind by an earlier run.
//
// A build is normally torn down at Close, so anything still here belongs to a
// process that never got to run it: a SIGKILL, a panicking host, a pulled
// plug. Without this they accumulate, and the disk they hold is only
// recoverable by hand.
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

		if p.spare[absDir(dir)] {
			slog.Info("workspace.sweep_spared", "dir", dir)

			continue
		}

		slog.Info("workspace.sweep_stale_build", "dir", dir)

		removeErr := p.backend.remove(dir)
		if removeErr != nil {
			slog.Warn("workspace.sweep_failed", "dir", dir, "error", removeErr)
		}
	}
}

// Reuse makes every NewBuild answer with dir: a resumed run is one run
// continuing, and splitting it back into fresh per-build directories would
// hide the artifacts it is resuming FOR.
func (p *isolatingProvider) Reuse(dir string) { p.reuse = dir }

func (p *isolatingProvider) Spare(dirs []string) {
	p.spare = map[string]bool{}

	for _, dir := range dirs {
		p.spare[absDir(dir)] = true
	}
}

// absDir is how a spared tree and a swept one are compared: a relative workspace.root: and the absolute path a run recorded name one directory.
func absDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return filepath.Clean(dir)
	}

	return abs
}

func (p *isolatingProvider) Resuming(dir string) Provider {
	return resumingProvider{isolatingProvider: p, dir: dir}
}

// resumingProvider shares everything with the provider it was made from but the tree its builds continue in.
type resumingProvider struct {
	*isolatingProvider
	dir string
}

func (r resumingProvider) NewBuild(ctx context.Context, _ string) (BuildWorkspace, error) {
	return r.reusedBuild(ctx, r.dir), nil
}

// Close is the shared provider's to call, never one run's.
func (resumingProvider) Close() error { return nil }

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
		return p.reusedBuild(ctx, p.reuse), nil
	}

	// The token comes before the counter because the counter alone is not
	// unique: it restarts at 1 in every process, so a crashed run's leftover
	// b-1-<label> would collide with the next run's — MkdirAll succeeds on the
	// existing directory and the backend then fails creating an artifact
	// directory that is already there. sweepStaleBuilds normally removes those
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
		keep: p.keep, cache: p.cache, stepCache: p.stepCache, taskCaches: p.taskCaches,
	}, nil
}

// reusedBuild continues in a tree that already has its artifacts/ and steps/ from the run being continued; per-step directories under steps/ are rebuilt per step anyway, which is what makes an isolating strategy resumable at all.
func (p *isolatingProvider) reusedBuild(ctx context.Context, dir string) *isolatingBuild {
	slog.DebugContext(ctx, "workspace.reuse", "dir", dir, "backend", "isolating")

	return &isolatingBuild{
		backend: p.backend, root: dir,
		artifacts:  filepath.Join(dir, "artifacts"),
		stepsDir:   filepath.Join(dir, "steps"),
		keep:       true, // never tear down a tree we did not create
		cache:      p.cache,
		stepCache:  p.stepCache,
		taskCaches: p.taskCaches,
	}
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
