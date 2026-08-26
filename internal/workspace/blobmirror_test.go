package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// memBlobs is a BlobStore over a directory: one subtree per digest. What the
// tests pin is the cache's use of the contract, not S3 — internal/blobstore
// owns that end.
type memBlobs struct {
	root string
	puts int
	gets int
}

func newMemBlobs(t *testing.T) *memBlobs {
	t.Helper()

	return &memBlobs{root: t.TempDir()}
}

func (m *memBlobs) dir(digest string) string { return filepath.Join(m.root, digest) }

func (m *memBlobs) HasTree(_ context.Context, digest string) (bool, error) {
	_, err := os.Stat(m.dir(digest))

	return err == nil, nil
}

func (m *memBlobs) PutTree(_ context.Context, digest, dir string) error {
	m.puts++

	return os.CopyFS(m.dir(digest), os.DirFS(dir)) //nolint:wrapcheck // a test fake
}

func (m *memBlobs) GetTree(_ context.Context, digest, dir string) error {
	_, err := os.Stat(m.dir(digest))
	if err != nil {
		return errors.New("blob not in the artifact store")
	}

	m.gets++

	return os.CopyFS(dir, os.DirFS(m.dir(digest))) //nolint:wrapcheck // a test fake
}

// plant installs arbitrary bytes under a digest, for the corruption test.
func (m *memBlobs) plant(t *testing.T, digest, content string) {
	t.Helper()

	err := os.MkdirAll(m.dir(digest), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(m.dir(digest), "junk.txt"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// memIndex is a StepIndex over a map.
type memIndex struct {
	entries map[string]map[string]string
}

func newMemIndex() *memIndex { return &memIndex{entries: map[string]map[string]string{}} }

func (m *memIndex) StepBlobs(_ context.Context, actionKey string) (map[string]string, error) {
	return m.entries[actionKey], nil
}

func (m *memIndex) RecordStepBlobs(_ context.Context, actionKey string, outputs map[string]string) error {
	m.entries[actionKey] = outputs

	return nil
}

// mirroredBuild is the harness: a durable-root provider with the mirror
// attached, holding one seeded input.
func mirroredBuild(t *testing.T, root string, blobs BlobStore, index StepIndex) BuildWorkspace {
	t.Helper()

	provider := stepCacheProvider(t, root)

	if !AttachArtifactStore(provider, blobs, index) {
		t.Fatal("AttachArtifactStore reported nothing attached against a durable root")
	}

	return seededBuild(t, provider, "input-content")
}

// mirrorRequest is the request shape the round-trip tests share.
func mirrorRequest() StepCacheRequest {
	return StepCacheRequest{ContentHash: "content-hash-1", Inputs: []string{"repo"}, Outputs: []string{"out"}}
}

// runAndStore produces the step's output and files it, returning the key.
func runAndStore(t *testing.T, bw BuildWorkspace, req StepCacheRequest) string {
	t.Helper()

	res := LookupStepCache(context.Background(), bw, req)
	if res.Hit {
		t.Fatal("a fresh cache reported a hit")
	}

	produceOutput(t, bw, req, "expensive-result")
	SaveStepCache(context.Background(), bw, res.Key, req)

	return res.Key
}

// TestStepCacheMirrorsAndRematerializes is the fleet-cache contract at this
// layer: a store publishes blobs and records the index, and a lookup whose
// local bytes are GONE materializes them back and reports a hit.
func TestStepCacheMirrorsAndRematerializes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blobs := newMemBlobs(t)
	index := newMemIndex()
	req := mirrorRequest()

	key := runAndStore(t, mirroredBuild(t, root, blobs, index), req)

	digest := index.entries[key]["out"]
	if digest == "" {
		t.Fatal("the store recorded no digest for the output")
	}

	if has, _ := blobs.HasTree(context.Background(), digest); !has {
		t.Fatal("the store published no blob under the recorded digest")
	}

	// Evict the local half: the shape of a pruned cache or another machine.
	err := os.RemoveAll(filepath.Join(root, stepCacheDirName))
	if err != nil {
		t.Fatal(err)
	}

	bw := mirroredBuild(t, root, blobs, index)

	res := LookupStepCache(context.Background(), bw, req)
	if !res.Hit {
		t.Fatal("a lookup with the bytes only in the blob store missed")
	}

	restored, err := os.ReadFile(filepath.Join(bw.(*isolatingBuild).artifacts, "out", "summary.md"))
	if err != nil || string(restored) != "expensive-result" {
		t.Fatalf("restored artifact = %q, %v; want the mirrored output", restored, err)
	}
}

// TestStepCacheRefusesABlobThatDigestsWrong pins the verification: the key is
// a claim, and bytes that do not digest to it are refused as a miss rather
// than installed under it.
func TestStepCacheRefusesABlobThatDigestsWrong(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blobs := newMemBlobs(t)
	index := newMemIndex()
	req := mirrorRequest()

	key := runAndStore(t, mirroredBuild(t, root, blobs, index), req)

	// Corrupt the mirror: same digest, different bytes.
	digest := index.entries[key]["out"]

	err := os.RemoveAll(blobs.dir(digest))
	if err != nil {
		t.Fatal(err)
	}

	blobs.plant(t, digest, "not what the digest says")

	err = os.RemoveAll(filepath.Join(root, stepCacheDirName))
	if err != nil {
		t.Fatal(err)
	}

	res := LookupStepCache(context.Background(), mirroredBuild(t, root, blobs, index), req)
	if res.Hit {
		t.Fatal("a blob whose bytes do not match its digest was served as a hit")
	}
}

// TestStepCacheMissesWhenTheMirrorHasNoAnswer covers the two quiet ends: an
// index that never heard of the key, and an index whose blob is gone. Both
// are ordinary misses — the step runs.
func TestStepCacheMissesWhenTheMirrorHasNoAnswer(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	blobs := newMemBlobs(t)
	index := newMemIndex()
	req := mirrorRequest()

	key := runAndStore(t, mirroredBuild(t, root, blobs, index), req)

	// The blob expired — a lifecycle rule, say — while the index row lives.
	err := os.RemoveAll(blobs.dir(index.entries[key]["out"]))
	if err != nil {
		t.Fatal(err)
	}

	err = os.RemoveAll(filepath.Join(root, stepCacheDirName))
	if err != nil {
		t.Fatal(err)
	}

	res := LookupStepCache(context.Background(), mirroredBuild(t, root, blobs, index), req)
	if res.Hit {
		t.Fatal("a hit was reported for a blob the store no longer holds")
	}
}

// TestAttachArtifactStoreNeedsADurableRoot pins the inert case: with a
// provider-owned temp root there is no step cache, so there is nothing to
// mirror and the attach says so.
func TestAttachArtifactStoreNeedsADurableRoot(t *testing.T) {
	t.Parallel()

	provider, err := NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	if AttachArtifactStore(provider, newMemBlobs(t), newMemIndex()) {
		t.Fatal("an artifact store attached to a provider with nowhere to mirror from")
	}
}
