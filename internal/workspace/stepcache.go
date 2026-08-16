package workspace

// The step output cache: reusing what a task or agent step produced, when the
// same work has already been done over the same bytes.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/jtarchie/steps/internal/config"
)

// stepCacheDirName is the directory under an isolating root that holds cached
// step outputs, alongside the resource cache. Neither starts with
// buildDirPrefix, so sweepStaleBuilds leaves both alone.
const stepCacheDirName = "steps-cache"

// defaultStepCacheMaxEntries bounds the step cache. Higher than the resource
// cache's ceiling because entries are per step per distinct input, not per
// resource version, so a handful of pipelines fills far more of them — and
// each is only as large as one step's declared outputs. Not configurable yet:
// nobody has run into the ceiling, and a knob added before that is a knob
// tuned by guesswork.
const defaultStepCacheMaxEntries = 200

// stepCacheDomain separates this cache's keys from every other hash in the
// codebase, so a merkle node hash can never be mistaken for an action key even
// if one were passed to the wrong function.
const stepCacheDomain = "steps/step-cache/v1"

// StepCacheRequest is one step's identity for cache purposes: what the plan
// says the step IS, and which artifacts it reads.
type StepCacheRequest struct {
	// ContentHash is the step's own hashed content (internal/merkle) — its
	// command or prompt, image, tool grant, declared inputs/outputs and
	// mappings. It identifies the WORK; the input digests below identify what
	// the work is being done to.
	ContentHash string
	// Inputs are the step's declared input names, and InputMapping renames a
	// declared name to the artifact it draws from (see materializeSpace).
	Inputs       []string
	InputMapping map[string]string
	// Outputs are the step's declared output names, and OutputMapping renames
	// a declared name to the artifact it is captured as. They are not part of
	// the key (ContentHash already folds both in) — they are what a hit has to
	// restore, and what a miss will later store.
	Outputs       []string
	OutputMapping map[string]string
}

// StepCacheResult is what a lookup decided: the key this step's work is filed
// under, and whether the outputs were already there and have been restored
// into the artifact store.
type StepCacheResult struct {
	Key string
	Hit bool
}

// NodeResult is what a reused step records in place of the result it did not
// produce.
//
// Recorded rather than left nil so a succeeded node cannot be read as a step
// that ran and produced nothing — for an agent step, one whose transcript and
// token cost are simply absent. The key is included because it is the only
// handle on which earlier work is being reused.
func (r StepCacheResult) NodeResult() map[string]any {
	return map[string]any{"reused": true, "cache_key": r.Key}
}

// StepCaching is the optional BuildWorkspace capability the pipeline and agent
// packages use to reuse a step's outputs across runs.
//
// Optional rather than on BuildWorkspace itself, and absent unless the
// pipeline configured a durable workspace.root: with no root there is nowhere
// for an entry to outlive the run that wrote it, and a "cache" discarded at
// the end of every run is only a slower way to run.
type StepCaching interface {
	// RestoreStep computes the step's action key and, when an entry for it
	// holds every declared output, materializes them into the artifact store.
	RestoreStep(ctx context.Context, req StepCacheRequest) (StepCacheResult, error)
	// StoreStep files the outputs a step just captured under key. Called only
	// after the step succeeded and space.Capture put its outputs in the store.
	StoreStep(ctx context.Context, key string, req StepCacheRequest) error
}

// LookupStepCache asks bw to restore a step's outputs, tolerating a build that
// has no cache at all. A build without the capability answers "miss, no key",
// which reads at the call site as "run the step, store nothing".
func LookupStepCache(ctx context.Context, bw BuildWorkspace, req StepCacheRequest) StepCacheResult {
	caching, ok := bw.(StepCaching)
	if !ok {
		return StepCacheResult{}
	}

	res, err := caching.RestoreStep(ctx, req)
	if err != nil {
		// A cache that cannot be read must never fail a build — the step
		// simply runs, which is what it would have done anyway.
		slog.Warn("workspace.step_cache_lookup_failed", "error", err)

		return StepCacheResult{}
	}

	return res
}

// SaveStepCache files a just-finished step's outputs under the key its lookup
// returned. A zero key means the lookup found no cache to file into, so there
// is nothing to do.
func SaveStepCache(ctx context.Context, bw BuildWorkspace, key string, req StepCacheRequest) {
	if key == "" {
		return
	}

	caching, ok := bw.(StepCaching)
	if !ok {
		return
	}

	err := caching.StoreStep(ctx, key, req)
	if err != nil {
		// Best-effort in the same way the resource cache is: failing to
		// RECORD work that already succeeded must not fail the run that did
		// it.
		slog.Warn("workspace.step_cache_store_failed", "key", key, "error", err)
	}
}

// stepCache stores one entry per action key: a plain directory holding one
// materialized tree per declared output, under its declared name.
//
// Storing under the DECLARED name rather than the mapped artifact name keeps
// an entry readable on its own terms — it holds what the step produced, and
// the mapping that decides where those outputs land in the artifact store is
// already folded into the key through the step's content hash.
type stepCache struct {
	backend treeBackend
	entries *entryStore
}

func newStepCache(backend treeBackend, root string, maxEntries int) (*stepCache, error) {
	// removeTree, not remove: an entry is a plain directory that CONTAINS the
	// backend's materialized trees, so on btrfs the nested subvolumes have to
	// be deleted before the directory itself can be.
	entries, err := newEntryStore(root, stepCacheDirName, maxEntries, backend.removeTree)
	if err != nil {
		return nil, err
	}

	return &stepCache{backend: backend, entries: entries}, nil
}

// actionKey identifies this step's work over these exact bytes.
//
// It is computed at RUN time, from the digests of the inputs actually
// materialized, rather than from the plan-time node hash — which identifies
// only what the pipeline DECLARES, never what its inputs contained. That
// distinction is the whole design: a node hash is sound for chain-skip, where
// nothing downstream of a skip ever executes, and unsound here, where later
// steps do run. An upstream step that re-ran and answered differently (an
// agent, or anything reaching past its declared inputs) leaves its node hash
// untouched but its output bytes changed, so a node-hash-keyed cache would
// serve this step a result derived from bytes that are no longer there.
func (c *stepCache) actionKey(digests func(string) (string, error), req StepCacheRequest) (string, error) {
	sum := sha256.New()

	digestField(sum, []byte(stepCacheDomain))
	digestField(sum, []byte(req.ContentHash))

	// Sorted, so a step's declaration order cannot change its key.
	inputs := slices.Clone(req.Inputs)
	slices.Sort(inputs)

	digestLength(sum, uint64(len(inputs)))

	for _, in := range inputs {
		digest, err := digests(mappedName(in, req.InputMapping))
		if err != nil {
			return "", err
		}

		digestField(sum, []byte(in))
		digestField(sum, []byte(digest))
	}

	return hashHex(sum), nil
}

// restore materializes every declared output from the entry into the artifact
// store, reporting whether it did.
//
// All-or-nothing on presence: an entry missing any declared output is treated
// as a miss and left alone, so a store that was interrupted halfway can only
// ever cost a re-run, never hand a step a partially-populated set of inputs.
func (c *stepCache) restore(ctx context.Context, path, artifacts string, req StepCacheRequest) bool {
	for _, out := range req.Outputs {
		_, err := os.Stat(filepath.Join(path, out))
		if err != nil {
			return false
		}
	}

	// The entry directory itself must exist even for a step with no declared
	// outputs, where the loop above checks nothing: for such a step the
	// entry's mere presence IS the recorded fact that this work has been done.
	_, err := os.Stat(path)
	if err != nil {
		return false
	}

	for _, out := range req.Outputs {
		err := c.restoreOutput(ctx, path, artifacts, out, req.OutputMapping)
		if err != nil {
			slog.Warn("workspace.step_cache_restore_failed", "entry", path, "output", out, "error", err)

			// Some outputs may already have landed. That is safe: they are
			// exactly what the step would have produced, and the step is about
			// to run and overwrite them.
			return false
		}
	}

	c.entries.touch(path)

	slog.Debug("workspace.step_cache_hit", "entry", path)

	return true
}

func (c *stepCache) restoreOutput(ctx context.Context, path, artifacts, out string, mapping map[string]string) error {
	src := filepath.Join(path, out)

	// The same guard the resource cache applies to its entries: a cache
	// directory a step reached by absolute path and replaced with a symlink
	// would otherwise have its target copied into the artifact store.
	err := rejectSymlinkSrc(src)
	if err != nil {
		return fmt.Errorf("cached output %q: %w", out, err)
	}

	dst := filepath.Join(artifacts, mappedName(out, mapping))

	err = c.backend.remove(dst)
	if err != nil {
		return fmt.Errorf("replacing artifact for %q: %w", out, err)
	}

	err = c.backend.materialize(ctx, src, dst)
	if err != nil {
		return fmt.Errorf("restoring output %q: %w", out, err)
	}

	return nil
}

// store files a succeeded step's captured outputs under key.
func (c *stepCache) store(ctx context.Context, path, artifacts string, req StepCacheRequest) error {
	err := c.backend.removeTree(path)
	if err != nil {
		return fmt.Errorf("replacing cache entry: %w", err)
	}

	err = os.MkdirAll(path, 0o750)
	if err != nil {
		return fmt.Errorf("creating cache entry: %w", err)
	}

	for _, out := range req.Outputs {
		src := filepath.Join(artifacts, mappedName(out, req.OutputMapping))

		err = rejectSymlinkSrc(src)
		if err != nil {
			return fmt.Errorf("output %q: %w", out, err)
		}

		err = c.backend.materialize(ctx, src, filepath.Join(path, out))
		if err != nil {
			return fmt.Errorf("caching output %q: %w", out, err)
		}
	}

	c.entries.prune()

	slog.Debug("workspace.step_cache_store", "entry", path)

	return nil
}

// RestoreStep implements StepCaching.
func (b *isolatingBuild) RestoreStep(ctx context.Context, req StepCacheRequest) (StepCacheResult, error) {
	if b.stepCache == nil {
		return StepCacheResult{}, nil
	}

	err := validateCachedNames(req)
	if err != nil {
		return StepCacheResult{}, err
	}

	key, err := b.stepCache.actionKey(b.artifactDigest, req)
	if err != nil {
		return StepCacheResult{}, err
	}

	path, ok := b.stepCache.entries.path(key)
	if !ok {
		return StepCacheResult{}, fmt.Errorf("unusable step cache key %q", key)
	}

	hit := b.stepCache.restore(ctx, path, b.artifacts, req)
	if hit {
		// Restoring replaced artifacts wholesale, so anything remembered about
		// their contents is now describing bytes that are gone.
		b.forgetDigests(artifactNames(req.Outputs, req.OutputMapping))
	}

	return StepCacheResult{Key: key, Hit: hit}, nil
}

// StoreStep implements StepCaching.
func (b *isolatingBuild) StoreStep(ctx context.Context, key string, req StepCacheRequest) error {
	if b.stepCache == nil {
		return nil
	}

	err := validateCachedNames(req)
	if err != nil {
		return err
	}

	path, ok := b.stepCache.entries.path(key)
	if !ok {
		return fmt.Errorf("unusable step cache key %q", key)
	}

	return b.stepCache.store(ctx, path, b.artifacts, req)
}

// validateCachedNames holds a request's declared names to the same rules
// materializeSpace and Capture enforce, so a name that could escape the
// artifact store cannot reach the cache directory either.
func validateCachedNames(req StepCacheRequest) error {
	for _, name := range req.Inputs {
		err := config.ValidateArtifactName(name)
		if err != nil {
			return fmt.Errorf("input %q: %w", name, err)
		}
	}

	for _, name := range req.Outputs {
		err := config.ValidateArtifactName(name)
		if err != nil {
			return fmt.Errorf("output %q: %w", name, err)
		}

		if mapped := mappedName(name, req.OutputMapping); mapped != name {
			err = config.ValidateArtifactPath(mapped)
			if err != nil {
				return fmt.Errorf("output %q (mapped to %q): %w", name, mapped, err)
			}
		}
	}

	return nil
}

// artifactNames renames declared names through a mapping, which is where the
// artifact store actually keeps them.
func artifactNames(declared []string, mapping map[string]string) []string {
	names := make([]string, 0, len(declared))
	for _, name := range declared {
		names = append(names, mappedName(name, mapping))
	}

	return names
}
