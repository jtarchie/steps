package workspace

// The step cache's remote half: entries mirrored to a content-addressed blob
// store, so bytes evicted locally — or never on this machine — can be
// materialized back instead of re-earned by running the step.
//
// The index is the cache index (index.go): an entry names its trees by digest, and a tree this machine's store lacks is asked of the mirror by that digest. Best-effort both ways: a mirror that cannot be read or written costs a re-run, never a build.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
)

// errBlobDigestMismatch is a fetch whose mismatch was already logged as one,
// so the generic fetch-failure line does not say it twice.
var errBlobDigestMismatch = errors.New("blob digest mismatch")

// BlobStore is remote bytes by content digest: internal/blobstore's shape,
// declared where it is consumed. The digest is digestTree's, which is what
// lets what came back be VERIFIED — a blob is never trusted to be what its
// key claims.
type BlobStore interface {
	HasTree(ctx context.Context, digest string) (bool, error)
	PutTree(ctx context.Context, digest, dir string) error
	GetTree(ctx context.Context, digest, dir string) error
}

func (p *isolatingProvider) AttachArtifactStore(blobs BlobStore) bool {
	if p.stepCache == nil {
		return false
	}

	p.stepCache.blobs = blobs

	return true
}

// rehydrate files each missing output's tree in the store from the blob store, by the digest its entry names, and reports whether every one landed. A false is an ordinary miss: the step runs.
func (c *stepCache) rehydrate(ctx context.Context, missing []string, records map[string]heldRecord) bool {
	if c.blobs == nil {
		return false
	}

	for _, out := range missing {
		record, ok := records[out]
		if !ok || record.Holder != "" || !c.fetchOutput(ctx, out, record.Digest) {
			return false
		}
	}

	slog.DebugContext(ctx, "workspace.blob_rehydrated", "outputs", len(missing))

	return true
}

// fetchOutput files one output's tree in the store, verified by re-digesting what arrived: the key is a claim, and a corrupted blob installed unchecked would serve wrong bytes under a right key.
func (c *stepCache) fetchOutput(ctx context.Context, out, digest string) bool {
	if c.store.has(digest) {
		return true
	}

	staged, err := stageTree(filepath.Join(c.store.dir, ".fetch"), c.backend.remove, func(tmp string) error {
		buildErr := c.backend.createEmpty(ctx, tmp)
		if buildErr != nil {
			return fmt.Errorf("staging: %w", buildErr)
		}

		buildErr = c.blobs.GetTree(ctx, digest, tmp)
		if buildErr != nil {
			return fmt.Errorf("fetching: %w", buildErr)
		}

		got, buildErr := digestTree(tmp)
		if buildErr != nil || got != digest {
			slog.WarnContext(ctx, "workspace.blob_digest_mismatch", "output", out, "want", digest, "got", got, "error", buildErr)

			return errBlobDigestMismatch
		}

		return nil
	})
	if err == nil {
		err = c.store.install(ctx, staged, digest)
	}

	if err != nil && !errors.Is(err, errBlobDigestMismatch) {
		slog.WarnContext(ctx, "workspace.blob_fetch_failed", "output", out, "digest", digest, "error", err)
	}

	return err == nil
}

// publish mirrors a just-filed entry's trees to the blob store. The entry is already indexed, so a tree that never lands costs another machine a miss, never a wrong tree.
func (c *stepCache) publish(ctx context.Context, records map[string]heldRecord) {
	if c.blobs == nil {
		return
	}

	for out, record := range records {
		dir, err := c.store.path(record.Digest)
		if err == nil {
			err = c.putIfAbsent(ctx, record.Digest, dir)
		}

		if err != nil {
			slog.WarnContext(ctx, "workspace.blob_publish_failed", "output", out, "digest", record.Digest, "error", err)

			return
		}
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
