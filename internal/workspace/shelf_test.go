package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shelvedBuild is a failed build under a store bounded to one byte, its one artifact filed by Shelve; it answers the build and that artifact's digest.
func shelvedBuild(t *testing.T, root string) (*isolatingBuild, string) {
	t.Helper()

	provider, err := newProvider(&Settings{Root: root, StoreMaxBytes: 1}, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}

	build := bw.(*isolatingBuild) //nolint:forcetypeassert // the provider's own build

	err = os.MkdirAll(filepath.Join(build.artifacts, "out"), 0o750)
	if err == nil {
		err = os.WriteFile(filepath.Join(build.artifacts, "out", "file"), []byte("kept for the resume"), 0o600)
	}

	if err == nil {
		err = build.Shelve(t.Context())
	}

	if err != nil {
		t.Fatal(err)
	}

	held, ok := build.remoteArtifact("out")
	if !ok || !build.shelf.has(held.Digest) {
		t.Fatalf("out was not filed in the store: %+v", held)
	}

	return build, held.Digest
}

// fileAnother files a second tree, which bounds a one-byte store and so evicts every tree it may.
func fileAnother(t *testing.T, build *isolatingBuild) {
	t.Helper()

	src := filepath.Join(t.TempDir(), "other")

	err := os.MkdirAll(src, 0o750)
	if err == nil {
		err = os.WriteFile(filepath.Join(src, "file"), []byte("another tree"), 0o600)
	}

	if err == nil {
		_, err = build.shelf.put(t.Context(), src)
	}

	if err != nil {
		t.Fatal(err)
	}
}

// TestAFailedRunsTreeOutlivesTheStoresBound: the bound evicts least recently used trees, and a failed run's are exactly those nobody has touched since; its pin keeps them for the resume.
func TestAFailedRunsTreeOutlivesTheStoresBound(t *testing.T) {
	t.Parallel()

	build, digest := shelvedBuild(t, t.TempDir())

	fileAnother(t, build)

	if !build.shelf.has(digest) {
		t.Error("the store's bound evicted a tree a failed run is waiting on")
	}
}

// TestAFailedRunsTreeOutlivesItsCacheEntry: a failed run's artifact often has the bytes of a cache entry's tree, and the entry being pruned must not take the tree the resume needs.
func TestAFailedRunsTreeOutlivesItsCacheEntry(t *testing.T) {
	t.Parallel()

	build, digest := shelvedBuild(t, t.TempDir())
	build.shelf.grace = 0

	build.shelf.release([]string{digest})

	if !build.shelf.has(digest) {
		t.Error("releasing a cache entry's tree removed one a failed run is waiting on")
	}
}

// TestAnExpiredPinKeepsNothing: past its lifetime a failed run's tree is ordinary bytes the bound may take, so a run never resumed does not hold disk forever.
func TestAnExpiredPinKeepsNothing(t *testing.T) {
	t.Parallel()

	build, digest := shelvedBuild(t, t.TempDir())

	pin, err := build.shelf.pinPath(digest, filepath.Base(build.root))
	if err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-shelfPinLifetime - time.Minute)

	err = os.Chtimes(pin, old, old)
	if err != nil {
		t.Fatal(err)
	}

	fileAnother(t, build)

	if build.shelf.has(digest) {
		t.Error("a pin past its lifetime still kept the tree")
	}
}

// TestASucceedingResumeGivesItsPinsBack: once the resumed build is green nothing waits on what its failed attempt filed.
func TestASucceedingResumeGivesItsPinsBack(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	build, digest := shelvedBuild(t, root)

	provider, err := newProvider(&Settings{Root: root, StoreMaxBytes: 1}, false)
	if err != nil {
		t.Fatal(err)
	}

	provider.Reuse(build.root)

	resumed, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}

	err = resumed.Close()
	if err != nil {
		t.Fatal(err)
	}

	if build.shelf.pinned(digest) {
		t.Error("the tree is still pinned after its run's resume succeeded")
	}
}

// TestAResumedBuildClonesAShelvedInputFromTheStore: the input comes back into the step's space proven against its digest, and the tree's artifacts/ stays empty.
func TestAResumedBuildClonesAShelvedInputFromTheStore(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	build, _ := shelvedBuild(t, root)

	provider, err := newProvider(&Settings{Root: root}, false)
	if err != nil {
		t.Fatal(err)
	}

	provider.Reuse(build.root)

	resumed, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}

	// The pipeline's pull dials a worker by name, and the store is no worker.
	err = resumed.RestoreHeld(func(holder, _ string) func(context.Context, string) error {
		t.Errorf("a record the store holds was handed to the worker pull as holder %q", holder)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	space, err := resumed.TaskSpace(t.Context(), "read", []string{"out"}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer CloseSpace(space, "read")

	data, err := os.ReadFile(filepath.Join(space.Dir(), "out", "file"))
	if err != nil || string(data) != "kept for the resume" {
		t.Errorf("the step read %q (%v), want the shelved bytes", data, err)
	}

	_, err = os.Stat(filepath.Join(build.artifacts, "out"))
	if !os.IsNotExist(err) {
		t.Errorf("artifacts/out is back in the tree (%v), want the input cloned straight from the store", err)
	}
}
