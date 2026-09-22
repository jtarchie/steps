package workspace

// Artifacts a worker holds on the build's behalf (steps#138, rung 2): what
// pulls them, what does not, and what a lost holder costs.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldTree records src as held elsewhere, with a pull that writes one file
// into dst and counts how often it was asked.
func heldTree(t *testing.T, bw BuildWorkspace) *int {
	t.Helper()

	holder, ok := bw.(RemoteHolder)
	if !ok {
		t.Fatal("an isolating build should implement RemoteHolder")
	}

	pulls := 0

	err := holder.HoldRemote("src", RemoteArtifact{
		Digest: strings.Repeat("ab", 32),
		Holder: "local:/somewhere",
		Pull: func(_ context.Context, dst string) error {
			pulls++

			return os.WriteFile(filepath.Join(dst, "f.txt"), []byte("held\n"), 0o600)
		},
	})
	if err != nil {
		t.Fatalf("HoldRemote: %v", err)
	}

	return &pulls
}

func newBuild(t *testing.T) BuildWorkspace {
	t.Helper()

	provider := newTestCopyProvider(t)

	bw, err := provider.NewBuild(context.Background(), "build")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = bw.Close() })

	return bw
}

// TestAHeldArtifactIsPulledOnceForItsFirstReader: the first step that
// materializes it pulls; the second reads the copy that landed.
func TestAHeldArtifactIsPulledOnceForItsFirstReader(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	for i := range 2 {
		space, err := bw.TaskSpace(context.Background(), "reader", []string{"src"}, nil, nil, nil)
		if err != nil {
			t.Fatalf("TaskSpace %d: %v", i, err)
		}

		content, err := os.ReadFile(filepath.Join(space.Dir(), "src", "f.txt"))
		if err != nil || string(content) != "held\n" {
			t.Fatalf("reader %d saw %q, %v", i, content, err)
		}

		_ = space.Close()
	}

	if *pulls != 1 {
		t.Errorf("pulled %d times for two readers, want once", *pulls)
	}
}

// TestALostHolderFailsTheReaderByName: the error names the artifact and the
// machine, and nothing runs against an empty directory.
func TestALostHolderFailsTheReaderByName(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)

	holder, _ := bw.(RemoteHolder)

	err := holder.HoldRemote("src", RemoteArtifact{
		Digest: strings.Repeat("cd", 32),
		Holder: "local:/gone",
		Pull:   func(context.Context, string) error { return errors.New("this worker does not hold the artifact") },
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = bw.TaskSpace(context.Background(), "reader", []string{"src"}, nil, nil, nil)
	if err == nil {
		t.Fatal("TaskSpace succeeded with its input on a worker that lost it")
	}

	for _, want := range []string{`"src"`, "local:/gone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestAHeldArtifactCountsAsPresentWithoutBeingPulled: a guard's view and a
// put's inputs: all see the artifact; neither listing pulls it.
func TestAHeldArtifactCountsAsPresentWithoutBeingPulled(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	build, _ := bw.(*isolatingBuild)

	names, err := build.allArtifacts()
	if err != nil || len(names) != 1 || names[0] != "src" {
		t.Errorf("allArtifacts = %v, %v; want [src]", names, err)
	}

	if *pulls != 0 {
		t.Errorf("listing the store pulled %d times, want never", *pulls)
	}

	space, err := bw.GuardSpace(context.Background(), "guard", []string{"src"}, nil, false)
	if err != nil {
		t.Fatalf("GuardSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	_, err = os.Stat(filepath.Join(space.Dir(), "src", "f.txt"))
	if err != nil {
		t.Errorf("the guard's view lacks the held input: %v", err)
	}
}

// TestTheResourceCachePullsWhatAPlacedGetLeftBehind: the cache lives here,
// so it is a reader — the entry it files is the worker's tree, not the empty
// directory the deferred fetch left.
func TestTheResourceCachePullsWhatAPlacedGetLeftBehind(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	first := cachingBuild(t, root, 10)

	pulls := 0

	fetch := func(string) error {
		holder, _ := first.(RemoteHolder)

		return holder.HoldRemote("src", RemoteArtifact{
			Digest: strings.Repeat("ef", 32),
			Holder: "local:/worker",
			Pull: func(_ context.Context, dst string) error {
				pulls++

				return os.WriteFile(filepath.Join(dst, "NOTES.txt"), []byte("from the worker"), 0o600)
			},
		})
	}

	_, err := first.FetchResource(context.Background(), "src", strings.Repeat("11", 32), fetch)
	if err != nil {
		t.Fatalf("FetchResource: %v", err)
	}

	if pulls != 1 {
		t.Fatalf("the cache pulled %d times, want once to file the entry", pulls)
	}

	second := cachingBuild(t, root, 10)

	dir, err := second.FetchResource(context.Background(), "src", strings.Repeat("11", 32), func(string) error {
		t.Fatal("the second build fetched instead of hitting the cache")

		return nil
	})
	if err != nil {
		t.Fatalf("FetchResource on the second build: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "NOTES.txt")) //nolint:gosec // a path under this test's own build
	if err != nil || string(content) != "from the worker" {
		t.Errorf("the cached entry holds %q, %v; want the worker's tree", content, err)
	}
}

// TestHoldRemoteRefusesAnUnsafeName: the name comes from the plan, but the
// path it becomes must stay inside the store all the same.
func TestHoldRemoteRefusesAnUnsafeName(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	holder, _ := bw.(RemoteHolder)

	err := holder.HoldRemote("../escape", RemoteArtifact{Digest: "d", Pull: func(context.Context, string) error { return nil }})
	if err == nil {
		t.Error("HoldRemote accepted a name that leaves the store")
	}

	if !strings.Contains(err.Error(), "escape") {
		t.Errorf("error %v does not name the offending name", err)
	}
}

// TestAPlacedSpaceLeavesAHeldInputOnItsHolder: nothing is pulled, the input
// is reported with its holder, and the step directory has no copy of it.
func TestAPlacedSpaceLeavesAHeldInputOnItsHolder(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	placed, ok := bw.(PlacedSpaces)
	if !ok {
		t.Fatal("an isolating build should implement PlacedSpaces")
	}

	space, remote, err := placed.PlacedTaskSpace(context.Background(), "consumer", []string{"src"}, []string{"out"}, nil, nil)
	if err != nil {
		t.Fatalf("PlacedTaskSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	if remote["src"].Holder != "local:/somewhere" {
		t.Errorf("remote = %v, want src on its holder", remote)
	}

	if *pulls != 0 {
		t.Errorf("pulled %d times for a placed step, want never", *pulls)
	}

	_, err = os.Stat(filepath.Join(space.Dir(), "src"))
	if err == nil {
		t.Error("the step directory holds a copy of an input left on its holder")
	}
}

// TestAPlacedSpacePullsAMappedInput: a renamed input hashes under the wrong
// name for the holder's entry, so it comes here and is materialized under
// its declared name as always.
func TestAPlacedSpacePullsAMappedInput(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	placed, _ := bw.(PlacedSpaces)

	space, remote, err := placed.PlacedTaskSpace(context.Background(), "consumer", []string{"code"}, nil, map[string]string{"code": "src"}, nil)
	if err != nil {
		t.Fatalf("PlacedTaskSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	if len(remote) != 0 {
		t.Errorf("remote = %v, want a mapped input pulled rather than left", remote)
	}

	if *pulls != 1 {
		t.Errorf("pulled %d times, want once", *pulls)
	}

	content, err := os.ReadFile(filepath.Join(space.Dir(), "code", "f.txt"))
	if err != nil || string(content) != "held\n" {
		t.Errorf("the mapped input reads %q, %v", content, err)
	}
}

// TestAPlacedSpaceDoesNotClobberAHeldInputItAlsoOutputs: read-modify-write
// over a held artifact leaves no empty output directory where the worker
// will place the input.
func TestAPlacedSpaceDoesNotClobberAHeldInputItAlsoOutputs(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	_ = heldTree(t, bw)

	placed, _ := bw.(PlacedSpaces)

	space, _, err := placed.PlacedTaskSpace(context.Background(), "editor", []string{"src"}, []string{"src"}, nil, nil)
	if err != nil {
		t.Fatalf("PlacedTaskSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	_, err = os.Stat(filepath.Join(space.Dir(), "src"))
	if err == nil {
		t.Error("an empty output directory was created over an input the worker will place")
	}
}

// TestACaptureSupersedesAHeldArtifact: a step that produces src HERE after a
// worker held an earlier src leaves the local copy as the artifact — the
// stale record must not pull the old tree over it.
func TestACaptureSupersedesAHeldArtifact(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	space, err := bw.TaskSpace(context.Background(), "producer", nil, []string{"src"}, nil, nil)
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	err = os.WriteFile(filepath.Join(space.Dir(), "src", "f.txt"), []byte("local\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = space.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	_ = space.Close()

	reader, err := bw.TaskSpace(context.Background(), "reader", []string{"src"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("reader TaskSpace: %v", err)
	}

	defer func() { _ = reader.Close() }()

	content, err := os.ReadFile(filepath.Join(reader.Dir(), "src", "f.txt"))
	if err != nil || string(content) != "local\n" {
		t.Errorf("the reader saw %q, %v; want what was captured here", content, err)
	}

	if *pulls != 0 {
		t.Errorf("pulled %d times after a local capture, want never", *pulls)
	}
}

// TestAResourceDirSupersedesAHeldArtifact is the get's shape of the same
// rule: a fresh resource directory replaces whatever a worker held under it.
func TestAResourceDirSupersedesAHeldArtifact(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	pulls := heldTree(t, bw)

	dir, err := bw.ResourceDir(context.Background(), "src")
	if err != nil {
		t.Fatalf("ResourceDir: %v", err)
	}

	err = os.WriteFile(filepath.Join(dir, "f.txt"), []byte("fetched here\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	reader, err := bw.TaskSpace(context.Background(), "reader", []string{"src"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("reader TaskSpace: %v", err)
	}

	defer func() { _ = reader.Close() }()

	content, err := os.ReadFile(filepath.Join(reader.Dir(), "src", "f.txt"))
	if err != nil || string(content) != "fetched here\n" {
		t.Errorf("the reader saw %q, %v; want the local fetch", content, err)
	}

	if *pulls != 0 {
		t.Errorf("pulled %d times after a local fetch, want never", *pulls)
	}
}

// TestAPlacedSpaceCapturesAnOutputItsWorkerKept: read-modify-write over a
// held input, where the worker keeps the result too — the step directory
// never had the tree, and Capture must not fail the step over that.
func TestAPlacedSpaceCapturesAnOutputItsWorkerKept(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)
	_ = heldTree(t, bw)

	placed, _ := bw.(PlacedSpaces)

	space, _, err := placed.PlacedTaskSpace(context.Background(), "editor", []string{"src"}, []string{"src"}, nil, nil)
	if err != nil {
		t.Fatalf("PlacedTaskSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	err = space.Capture(context.Background())
	if err != nil {
		t.Fatalf("Capture of an output the worker kept: %v", err)
	}

	holder, _ := bw.(RemoteHolder)

	err = holder.HoldRemote("src", RemoteArtifact{
		Digest: strings.Repeat("ef", 32),
		Holder: "local:/somewhere",
		Pull:   func(context.Context, string) error { return nil },
	})
	if err != nil {
		t.Fatalf("HoldRemote after the capture: %v", err)
	}

	build, _ := bw.(*isolatingBuild)
	if _, held := build.remoteArtifact("src"); !held {
		t.Error("the edited tree is not recorded on its worker")
	}
}

// TestAPlacedSpacePullsAnInputTheHolderFiledUnderAnotherName: a producer's
// output_mapping files the tree as `out` and captures it as `x`; the digest
// binds the filed name, so x cannot be offered by digest and comes here.
func TestAPlacedSpacePullsAnInputTheHolderFiledUnderAnotherName(t *testing.T) {
	t.Parallel()

	bw := newBuild(t)

	holder, _ := bw.(RemoteHolder)
	pulls := 0

	err := holder.HoldRemote("x", RemoteArtifact{
		Name:   "out",
		Digest: strings.Repeat("ab", 32),
		Holder: "local:/somewhere",
		Pull: func(_ context.Context, dst string) error {
			pulls++

			return os.WriteFile(filepath.Join(dst, "f.txt"), []byte("held\n"), 0o600)
		},
	})
	if err != nil {
		t.Fatalf("HoldRemote: %v", err)
	}

	placed, _ := bw.(PlacedSpaces)

	space, remote, err := placed.PlacedTaskSpace(context.Background(), "consumer", []string{"x"}, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlacedTaskSpace: %v", err)
	}

	defer func() { _ = space.Close() }()

	if len(remote) != 0 {
		t.Errorf("remote = %v, want an input filed under another name pulled rather than left", remote)
	}

	if pulls != 1 {
		t.Errorf("pulled %d times, want once", pulls)
	}

	content, err := os.ReadFile(filepath.Join(space.Dir(), "x", "f.txt"))
	if err != nil || string(content) != "held\n" {
		t.Errorf("the input reads %q, %v", content, err)
	}
}
