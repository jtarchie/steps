package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func hostBuild(t *testing.T, root string) BuildWorkspace {
	t.Helper()

	provider, err := NewProvider(&Settings{Root: root}, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	build, err := provider.NewBuild(t.Context(), "build")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = build.Close() })

	return build
}

// TestAHeldStablePathIsNeverShared: a second claim while the first space is open takes a throwaway path and leaves the first one's files alone.
func TestAHeldStablePathIsNeverShared(t *testing.T) {
	t.Parallel()

	build := hostBuild(t, t.TempDir())
	key := StableWorkKey("p", "j", "s")

	first, err := build.HostTaskSpace(t.Context(), key, "s", nil, []string{"out"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer CloseSpace(first, "s")

	mine := filepath.Join(first.Dir(), "out", "mine")

	err = os.WriteFile(mine, []byte("first"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	second, err := build.HostTaskSpace(t.Context(), key, "s", nil, []string{"out"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer CloseSpace(second, "s")

	if second.Dir() == first.Dir() {
		t.Fatalf("both spaces got %s", first.Dir())
	}

	_, err = os.Stat(mine)
	if err != nil {
		t.Errorf("the second claim took the first space's files: %v", err)
	}
}

// TestAStablePathIsEmptiedOfWhatACrashLeft: a holder killed before its Close leaves files at the path, and the next holder must not see them.
func TestAStablePathIsEmptiedOfWhatACrashLeft(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	key := StableWorkKey("p", "j", "s")
	leftover := filepath.Join(root, workDirName, key, "leftover")

	err := os.MkdirAll(filepath.Dir(leftover), 0o750)
	if err == nil {
		err = os.WriteFile(leftover, nil, 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	space, err := hostBuild(t, root).HostTaskSpace(t.Context(), key, "s", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	defer CloseSpace(space, "s")

	if space.Dir() != filepath.Dir(leftover) {
		t.Fatalf("the space is at %s, want the free stable path %s", space.Dir(), filepath.Dir(leftover))
	}

	entries, err := os.ReadDir(space.Dir())
	if err != nil || len(entries) != 0 {
		t.Errorf("the stable path holds %v (%v), want it empty", entries, err)
	}
}
