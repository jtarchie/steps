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

// heldTree records an artifact as held elsewhere, with a pull that writes
// content into dst and counts how often it was asked.
func heldTree(t *testing.T, bw BuildWorkspace, name, content string) *int {
	t.Helper()

	holder, ok := bw.(RemoteHolder)
	if !ok {
		t.Fatal("an isolating build should implement RemoteHolder")
	}

	pulls := 0

	err := holder.HoldRemote(name, RemoteArtifact{
		Digest: strings.Repeat("ab", 32),
		Holder: "local:/somewhere",
		Pull: func(_ context.Context, dst string) error {
			pulls++

			return os.WriteFile(filepath.Join(dst, "f.txt"), []byte(content), 0o600)
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
	pulls := heldTree(t, bw, "src", "held\n")

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
	pulls := heldTree(t, bw, "src", "held\n")

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
