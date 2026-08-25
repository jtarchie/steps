package workspace

// The step cache's remote half: entries mirrored to a content-addressed blob
// store, so bytes evicted locally — or never on this machine — can be
// materialized back instead of re-earned by running the step.
//
// Both halves arrive as interfaces this package declares, because neither
// implementation may be imported from here: the blobs are internal/blobstore
// (an AWS SDK), the index is internal/store (SQLite), and workspace's depguard
// allow-list names neither. main wires them in through AttachArtifactStore.
// Everything here is best-effort in both directions — a mirror that cannot be
// read costs a re-run, one that cannot be written costs the next machine a
// re-run, and neither may fail the build that is otherwise working.

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// BlobStore is remote bytes by content digest: internal/blobstore's shape,
// declared where it is consumed. The digest is digestTree's, which is what
// lets what came back be VERIFIED — a blob is never trusted to be what its
// key claims.
type BlobStore interface {
	HasTree(ctx context.Context, digest string) (bool, error)
	PutTree(ctx context.Context, digest, dir string) error
	GetTree(ctx context.Context, digest, dir string) error
}

// StepIndex is the durable action-key-to-output-digest mapping:
// internal/store's shape. The index is truth and stays on the orchestrator;
// the blob store holds bytes and nothing else.
type StepIndex interface {
	StepBlobs(ctx context.Context, actionKey string) (map[string]string, error)
	RecordStepBlobs(ctx context.Context, actionKey string, outputs map[string]string) error
}

// ArtifactStoreAttacher is the optional Provider capability behind
// AttachArtifactStore, following StepCaching's type-assertion pattern.
type ArtifactStoreAttacher interface {
	AttachArtifactStore(blobs BlobStore, index StepIndex) bool
}

// AttachArtifactStore wires the two halves of the artifact store into p's
// step cache, reporting whether anything was attached — false for a provider
// without the capability, or one with no durable root, where there is no
// local cache to mirror.
func AttachArtifactStore(p Provider, blobs BlobStore, index StepIndex) bool {
	attacher, ok := p.(ArtifactStoreAttacher)
	if !ok {
		return false
	}

	return attacher.AttachArtifactStore(blobs, index)
}

// AttachArtifactStore implements ArtifactStoreAttacher.
func (p *isolatingProvider) AttachArtifactStore(blobs BlobStore, index StepIndex) bool {
	if p.stepCache == nil {
		return false
	}

	p.stepCache.blobs = blobs
	p.stepCache.index = index

	return true
}

// rehydrate materializes an entry's missing outputs from the blob store,
// reporting whether every one of them landed. A false is an ordinary miss:
// the step runs, and its store refills both halves.
func (c *stepCache) rehydrate(ctx context.Context, key, path string, missing []string) bool {
	if c.blobs == nil || c.index == nil {
		return false
	}

	digests, err := c.index.StepBlobs(ctx, key)
	if err != nil {
		slog.Warn("workspace.blob_index_lookup_failed", "key", key, "error", err)

		return false
	}

	if len(digests) == 0 {
		return false
	}

	for _, out := range missing {
		digest, ok := digests[out]
		if !ok || digest == "" {
			return false
		}

		if !c.fetchOutput(ctx, path, out, digest) {
			return false
		}
	}

	slog.Debug("workspace.blob_rehydrated", "entry", path, "outputs", len(missing))

	return true
}

// fetchOutput lands one output in the entry, staged and verified first.
//
// Unpacked into a backend-created tree rather than a plain directory, because
// the entry's outputs are what restore later snapshots — and a btrfs snapshot
// of a plain directory is an error. Verified by re-digesting what arrived:
// the key is a claim, and a corrupted or truncated blob installed unchecked
// would serve wrong bytes under a right key for as long as the entry lives.
func (c *stepCache) fetchOutput(ctx context.Context, path, out, digest string) bool {
	err := os.MkdirAll(path, 0o750)
	if err != nil {
		slog.Warn("workspace.blob_fetch_failed", "output", out, "error", err)

		return false
	}

	tmp := filepath.Join(path, out+stagedSuffix)

	cleanup := func() {
		removeErr := c.backend.remove(tmp)
		if removeErr != nil {
			slog.Warn("workspace.blob_staging_cleanup", "dir", tmp, "error", removeErr)
		}
	}

	cleanup()

	err = c.backend.createEmpty(ctx, tmp)
	if err == nil {
		err = c.blobs.GetTree(ctx, digest, tmp)
	}

	if err != nil {
		slog.Warn("workspace.blob_fetch_failed", "output", out, "digest", digest, "error", err)
		cleanup()

		return false
	}

	got, err := digestTree(tmp)
	if err != nil || got != digest {
		slog.Warn("workspace.blob_digest_mismatch", "output", out, "want", digest, "got", got, "error", err)
		cleanup()

		return false
	}

	err = os.Rename(tmp, filepath.Join(path, out))
	if err != nil {
		slog.Warn("workspace.blob_fetch_failed", "output", out, "error", err)
		cleanup()

		return false
	}

	return true
}

// publish mirrors a just-stored entry to the blob store and records the
// digests in the index — after the local store succeeded, best-effort, and
// index-last: an index row naming blobs that never landed would promise a
// rehydration that must fail, while blobs without a row merely wait for the
// next record.
func (c *stepCache) publish(ctx context.Context, key, path string, digests func(string) (string, error), req StepCacheRequest) {
	if c.blobs == nil || c.index == nil {
		return
	}

	outputs := make(map[string]string, len(req.Outputs))

	for _, out := range req.Outputs {
		digest, err := digests(mappedName(out, req.OutputMapping))
		if err != nil || digest == "" {
			slog.Warn("workspace.blob_publish_failed", "output", out, "error", err)

			return
		}

		err = c.putIfAbsent(ctx, digest, filepath.Join(path, out))
		if err != nil {
			slog.Warn("workspace.blob_publish_failed", "output", out, "digest", digest, "error", err)

			return
		}

		outputs[out] = digest
	}

	err := c.index.RecordStepBlobs(ctx, key, outputs)
	if err != nil {
		slog.Warn("workspace.blob_index_record_failed", "key", key, "error", err)
	}
}

// putIfAbsent uploads one tree unless the store already holds its digest —
// the whole point of content addressing is that a key never needs
// re-uploading.
func (c *stepCache) putIfAbsent(ctx context.Context, digest, dir string) error {
	has, err := c.blobs.HasTree(ctx, digest)
	if err != nil {
		return fmt.Errorf("checking the blob store: %w", err)
	}

	if has {
		return nil
	}

	err = c.blobs.PutTree(ctx, digest, dir)
	if err != nil {
		return fmt.Errorf("uploading to the blob store: %w", err)
	}

	return nil
}
