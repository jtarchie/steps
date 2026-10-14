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
// if one were passed to the wrong function. v2: input digests became
// treedigest's, so every v1 entry is keyed by a name no tree has any more.
const stepCacheDomain = "steps/step-cache/v2"

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

// LookupStepCache asks bw to restore a step's outputs, reading an error as
// "miss, no key" — at the call site, "run the step, store nothing".
func LookupStepCache(ctx context.Context, bw BuildWorkspace, req StepCacheRequest) StepCacheResult {
	res, err := bw.RestoreStep(ctx, req)
	if err != nil {
		// A cache that cannot be read must never fail a build — the step
		// simply runs, which is what it would have done anyway.
		slog.WarnContext(ctx, "workspace.step_cache_lookup_failed", "error", err)

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

	err := bw.StoreStep(ctx, key, req)
	if err != nil {
		// Best-effort in the same way the resource cache is: failing to
		// RECORD work that already succeeded must not fail the run that did
		// it.
		slog.WarnContext(ctx, "workspace.step_cache_store_failed", "key", key, "error", err)
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
	// blobs and index are the cache's remote mirror, nil unless an
	// --artifact-store was attached — see blobmirror.go.
	blobs BlobStore
	index StepIndex
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
// Two-phase, and that is the whole point of the shape. Every output is first
// materialized to a staging path BESIDE its destination; only once all of them
// have landed is any existing artifact replaced. Restoring in place instead
// would mean an entry evicted mid-restore (or a full disk, or a refused
// snapshot) had already deleted a destination it could not refill — and since
// a failed restore is reported as a plain miss, the step would then run
// against an artifact store missing one of its own declared inputs and fail
// the build. A cache failure has to cost a re-run and nothing else.
//
// All-or-nothing on presence too: an entry missing any declared output is
// treated as a miss and left alone, so a store interrupted halfway can only
// ever cost a re-run.
func (c *stepCache) restore(ctx context.Context, key, path, artifacts string, req StepCacheRequest) bool {
	// A request with no outputs has nothing to restore, so every check below
	// passes vacuously and this would report a hit against an empty cache —
	// silently skipping a step whose entire result is its side effect.
	// merkle.StepCacheable already refuses such a step; this is the same rule
	// held at the layer that decides what an entry MEANS, so a future caller
	// cannot reintroduce it.
	if len(req.Outputs) == 0 {
		return false
	}

	// Local-first: an output whose bytes are here is never re-fetched. Only
	// what is MISSING — a pruned entry, a fresh machine handed this state —
	// is asked of the blob mirror, and a mirror that cannot answer leaves
	// this a plain miss.
	missing := make([]string, 0, len(req.Outputs))

	for _, out := range req.Outputs {
		_, err := os.Stat(filepath.Join(path, out))
		if err != nil {
			missing = append(missing, out)
		}
	}

	if len(missing) > 0 && !c.rehydrate(ctx, key, path, missing) {
		return false
	}

	staged, err := c.stageOutputs(ctx, path, artifacts, req)
	defer c.discardStaged(staged)

	if err != nil {
		slog.WarnContext(ctx, "workspace.step_cache_restore_failed", "entry", path, "error", err)

		return false
	}

	err = c.commitStaged(staged)
	if err != nil {
		// Past the point of no return: some artifacts are replaced and one is
		// not. Reporting a miss is still right — the step runs and rewrites
		// its own outputs — but the caller must forget every digest it
		// remembered for them, which RestoreStep does unconditionally.
		slog.WarnContext(ctx, "workspace.step_cache_commit_failed", "entry", path, "error", err)

		return false
	}

	c.entries.touch(path)

	slog.DebugContext(ctx, "workspace.step_cache_hit", "entry", path)

	return true
}

// stagedOutput is one output materialized next to where it is going.
type stagedOutput struct{ tmp, dst string }

// stageOutputs materializes every declared output beside its destination,
// touching no existing artifact.
func (c *stepCache) stageOutputs(ctx context.Context, path, artifacts string, req StepCacheRequest) ([]stagedOutput, error) {
	staged := make([]stagedOutput, 0, len(req.Outputs))

	for _, out := range req.Outputs {
		src := filepath.Join(path, out)

		// The same guard the resource cache applies to its entries: a cache
		// directory a step reached by absolute path and replaced with a
		// symlink would otherwise have its target copied into the artifact
		// store.
		err := rejectSymlinkSrc(src)
		if err != nil {
			return staged, fmt.Errorf("cached output %q: %w", out, err)
		}

		dst := filepath.Join(artifacts, mappedName(out, req.OutputMapping))

		tmp, err := stageTree(dst, c.backend.remove, func(tmp string) error {
			return c.backend.materialize(ctx, src, tmp)
		})
		if err != nil {
			return staged, fmt.Errorf("staging output %q: %w", out, err)
		}

		staged = append(staged, stagedOutput{tmp: tmp, dst: dst})
	}

	return staged, nil
}

// commitStaged swaps each staged output into place. Only local renames remain
// here, so the window in which an artifact is absent is as short as this
// design can make it.
func (c *stepCache) commitStaged(staged []stagedOutput) error {
	for i, out := range staged {
		err := swapIn(out.tmp, out.dst, c.backend.remove)
		if err != nil {
			return fmt.Errorf("installing artifact %q: %w", filepath.Base(out.dst), err)
		}

		staged[i].tmp = "" // committed; discardStaged must not remove it
	}

	return nil
}

// discardStaged removes whatever is left staged, on every path out of restore.
func (c *stepCache) discardStaged(staged []stagedOutput) {
	for _, out := range staged {
		if out.tmp != "" {
			discardTree(out.tmp, c.backend.remove)
		}
	}
}

// store files a succeeded step's captured outputs under key, then mirrors
// the entry to the blob store when one is attached.
//
// Built whole beside path and renamed into place, because restore reads an
// output's mere presence as cached: an entry filled in place and interrupted
// mid-copy would serve an empty output as a hit.
func (c *stepCache) store(ctx context.Context, key, path, artifacts string, digests func(string) (string, error), req StepCacheRequest) error {
	err := replaceTree(path, c.backend.removeTree, func(staged string) error {
		mkErr := os.MkdirAll(staged, 0o750)
		if mkErr != nil {
			return fmt.Errorf("creating cache entry: %w", mkErr)
		}

		for _, out := range req.Outputs {
			src := filepath.Join(artifacts, mappedName(out, req.OutputMapping))

			symErr := rejectSymlinkSrc(src)
			if symErr != nil {
				return fmt.Errorf("output %q: %w", out, symErr)
			}

			matErr := c.backend.materialize(ctx, src, filepath.Join(staged, out))
			if matErr != nil {
				return fmt.Errorf("caching output %q: %w", out, matErr)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	c.entries.prune()
	c.publish(ctx, key, path, digests, req)

	slog.DebugContext(ctx, "workspace.step_cache_store", "entry", path)

	return nil
}

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

	hit := b.stepCache.restore(ctx, key, path, b.artifacts, req)

	if hit {
		// The restored outputs are here now, whatever a worker still holds.
		b.forgetRemote(artifactNames(req.Outputs, req.OutputMapping))
	}

	// Unconditionally, not only on a hit: a restore that failed PART WAY has
	// still replaced some artifacts, and a digest remembered for one of those
	// would describe bytes that are gone — which is precisely how a later
	// step's action key comes out right for content that is no longer there.
	b.forgetDigests(artifactNames(req.Outputs, req.OutputMapping))

	return StepCacheResult{Key: key, Hit: hit}, nil
}

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

	// The entry lives on this disk, so the cache is a local reader: outputs
	// a placed step left on its worker come home to be filed.
	for _, out := range req.Outputs {
		err = b.ensureLocal(ctx, mappedName(out, req.OutputMapping))
		if err != nil {
			return err
		}
	}

	return b.stepCache.store(ctx, key, path, b.artifacts, b.artifactDigest, req)
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
		err := validateOutputName(name, req.OutputMapping)
		if err != nil {
			return err
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
