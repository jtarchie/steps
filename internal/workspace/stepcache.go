package workspace

// The step output cache: reusing what a task or agent step produced, when the
// same work has already been done over the same bytes.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"maps"
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

// stepCache keeps one pointer per action key, naming where each declared output's tree is: this machine's store, or a docker. Filed by DECLARED name, since the mapping is already in the key through the step's content hash.
type stepCache struct {
	backend treeBackend
	store   *treeStore
	entries *entryStore
	// blobs and index are the remote mirror (blobmirror.go), nil without --artifact-store.
	blobs BlobStore
	index StepIndex
}

func newStepCache(backend treeBackend, store *treeStore, root string, maxEntries int) (*stepCache, error) {
	entries, err := newEntryStore(root, stepCacheDirName, maxEntries, backend.remove)
	if err != nil {
		return nil, err
	}

	return &stepCache{backend: backend, store: store, entries: entries}, nil
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

// restore materializes every declared output the entry names into the artifact store, all or nothing: each is staged beside its destination first, so an evicted or corrupt tree costs a re-run and never an artifact store missing a declared input. records are the entry's, its docker-held ones already claimed; those RestoreStep hands on.
func (c *stepCache) restore(ctx context.Context, key, path, artifacts string, req StepCacheRequest, records map[string]heldRecord) bool {
	// With no outputs every check passes vacuously, and a hit would silently skip a step whose whole result is its side effect.
	if len(req.Outputs) == 0 {
		return false
	}

	// Local-first: only what the store lacks is asked of the blob mirror.
	missing := make([]string, 0, len(req.Outputs))

	for _, out := range req.Outputs {
		record, ok := records[out]
		if ok && (record.Holder != "" || c.store.has(record.Digest)) {
			continue
		}

		missing = append(missing, out)
	}

	if len(missing) > 0 {
		if !c.rehydrate(ctx, key, missing, records) {
			return false
		}

		_ = c.point(ctx, path, records) // logged there; the trees are in the store either way
	}

	staged, err := c.stageOutputs(ctx, artifacts, req, records)
	defer c.discardStaged(staged)

	if err != nil {
		slog.WarnContext(ctx, "workspace.step_cache_restore_failed", "entry", path, "error", err)

		return false
	}

	err = c.commitStaged(staged)
	if err != nil {
		// Some artifacts are replaced and one is not; still a miss, and RestoreStep forgets every digest it remembered for them.
		slog.WarnContext(ctx, "workspace.step_cache_commit_failed", "entry", path, "error", err)

		return false
	}

	c.entries.touch(path)

	slog.DebugContext(ctx, "workspace.step_cache_hit", "entry", path)

	return true
}

// stagedOutput is one output materialized next to where it is going.
type stagedOutput struct{ tmp, dst string }

// stageOutputs materializes every stored output beside its destination, touching no existing artifact.
func (c *stepCache) stageOutputs(ctx context.Context, artifacts string, req StepCacheRequest, records map[string]heldRecord) ([]stagedOutput, error) {
	staged := make([]stagedOutput, 0, len(req.Outputs))

	for _, out := range req.Outputs {
		record := records[out]
		if record.Holder != "" {
			continue
		}

		dst := filepath.Join(artifacts, mappedName(out, req.OutputMapping))

		tmp, err := stageTree(dst, c.backend.remove, func(tmp string) error {
			return c.store.materialize(ctx, record.Digest, tmp)
		})
		if err != nil {
			return staged, fmt.Errorf("staging output %q: %w", out, err)
		}

		staged = append(staged, stagedOutput{tmp: tmp, dst: dst})
	}

	return staged, nil
}

// commitStaged swaps each staged output into place; only renames are left here, so an artifact is absent for as short a window as this design allows.
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

// file puts a succeeded step's captured outputs in the store and points the entry at them, beside the ones docker holds (held). An entry with a held output is not mirrored: the mirror needs the bytes here.
func (c *stepCache) file(ctx context.Context, key, path, artifacts string, req StepCacheRequest, held map[string]heldRecord) error {
	records := maps.Clone(held)
	if records == nil {
		records = map[string]heldRecord{}
	}

	for _, out := range req.Outputs {
		if _, ok := held[out]; ok {
			continue
		}

		digest, err := c.store.put(ctx, filepath.Join(artifacts, mappedName(out, req.OutputMapping)))
		if err != nil {
			return fmt.Errorf("caching output %q: %w", out, err)
		}

		records[out] = heldRecord{Digest: digest}
	}

	err := c.point(ctx, path, records)
	if err != nil {
		return err
	}

	c.entries.prune()
	c.store.collect()

	if len(held) == 0 {
		c.publish(ctx, key, records)
	}

	slog.DebugContext(ctx, "workspace.step_cache_store", "entry", path)

	return nil
}

// point replaces the entry with one naming records, built beside it and renamed into place so an interrupted write is never read as an entry.
func (c *stepCache) point(ctx context.Context, path string, records map[string]heldRecord) error {
	err := replaceTree(path, c.backend.remove, func(staged string) error {
		mkErr := os.MkdirAll(staged, 0o750)
		if mkErr != nil {
			return fmt.Errorf("creating cache entry: %w", mkErr)
		}

		return writeHeld(staged, records)
	})
	if err != nil {
		slog.WarnContext(ctx, "workspace.step_cache_store_failed", "entry", path, "error", err)
	}

	return err
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

	// Claimed before anything is replaced: a held tree docker has since evicted makes the whole entry a miss, which costs a run and nothing else.
	records, err := claimEntry(ctx, path)
	if err != nil {
		slog.WarnContext(ctx, "workspace.step_cache_held_miss", "entry", path, "error", err)

		return StepCacheResult{Key: key}, nil
	}

	held := remoteOnly(records)
	hit := b.stepCache.restore(ctx, key, path, b.artifacts, req, records)

	if hit {
		// The restored outputs are here now, whatever a worker still holds.
		b.forgetRemote(artifactNames(unheld(req, held), req.OutputMapping))
	}

	// Unconditionally: a restore that failed part way has still replaced some artifacts, and a digest remembered for one would key a later step for bytes that are gone.
	b.forgetDigests(artifactNames(req.Outputs, req.OutputMapping))

	if hit {
		// The held outputs are where the step that made them left them; the next container step mounts them there.
		err = b.holdRestored(ctx, held, req.OutputMapping)
		if err != nil {
			slog.WarnContext(ctx, "workspace.step_cache_held_miss", "entry", path, "error", err)

			return StepCacheResult{Key: key}, nil
		}
	}

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

	// An output docker holds is filed as where it is, so it stays there; the rest come home to the store, as they must with no way to reach docker or an artifact store needing the bytes.
	held := b.heldOutputs(ctx, req)

	for _, out := range req.Outputs {
		if _, ok := held[out]; ok {
			continue
		}

		err = b.ensureLocal(ctx, mappedName(out, req.OutputMapping))
		if err != nil {
			return err
		}
	}

	return b.stepCache.file(ctx, key, path, b.artifacts, req, held)
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
