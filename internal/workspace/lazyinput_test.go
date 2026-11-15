package workspace

// A container step whose input docker already holds never reads this machine's copy of it, and cloning 100k files cost ~20s per step (steps#246). These pin the deferred copy: none until a venue asks, the digest from the artifact itself, and an output still copied up front.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type lazyInputs interface {
	inputDigests
	Materialize(ctx context.Context, name string) error
}

func TestALazyInputIsCopiedOnlyWhenAVenueAsks(t *testing.T) {
	t.Parallel()

	bw := memoBuild(t)
	writeAndCapture(t, bw, "seed", nil, "aaaa")

	space, _, err := bw.PlacedTaskSpace(context.Background(), "reader", []string{"tree"}, nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = space.Close() })

	_, err = os.Stat(filepath.Join(space.Dir(), "tree", "file.txt"))
	if err == nil {
		t.Fatal("a lazy input was copied before anything asked for it")
	}

	lazy, ok := space.(lazyInputs)
	if !ok {
		t.Fatal("a lazy space cannot be asked to copy its inputs")
	}

	assertArtifactDigest(t, lazy, space.(*isolatingSpace).artifacts)

	for range 2 {
		err = lazy.Materialize(context.Background(), "tree")
		if err != nil {
			t.Fatal(err)
		}
	}

	body, err := os.ReadFile(filepath.Join(space.Dir(), "tree", "file.txt"))
	if err != nil || string(body) != "aaaa" {
		t.Errorf("after Materialize the input reads %q, %v; want the artifact's bytes", body, err)
	}
}

// assertArtifactDigest: an uncopied input's digest is walked off the artifact, since there is no copy to walk.
func assertArtifactDigest(t *testing.T, lazy lazyInputs, artifacts string) {
	t.Helper()

	want, err := digestTree(filepath.Join(artifacts, "tree"))
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := lazy.KnownDigest("tree"); !ok || got != want {
		t.Errorf("known digest of an uncopied input = %q, %v; want the artifact's own %q", got, ok, want)
	}
}

func TestAnInputThatIsAlsoAnOutputIsNeverLazy(t *testing.T) {
	t.Parallel()

	bw := memoBuild(t)
	writeAndCapture(t, bw, "seed", nil, "aaaa")

	space, _, err := bw.PlacedTaskSpace(context.Background(), "editor", []string{"tree"}, []string{"tree"}, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = space.Close() })

	// Its home copy is what the step's changes are applied over, and what Capture writes back.
	body, err := os.ReadFile(filepath.Join(space.Dir(), "tree", "file.txt"))
	if err != nil || string(body) != "aaaa" {
		t.Errorf("an edited input reads %q, %v; want it copied up front", body, err)
	}
}
