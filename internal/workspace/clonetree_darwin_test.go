//go:build darwin

package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cloneSource is a tree with what a clone must keep: a nested file, an executable, a symlink.
func cloneSource(t *testing.T) string {
	t.Helper()

	src := t.TempDir()

	err := os.MkdirAll(filepath.Join(src, "nested"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	for name, mode := range map[string]os.FileMode{"nested/data.txt": 0o600, "run.sh": 0o700} {
		err = os.WriteFile(filepath.Join(src, name), []byte(name), mode)
		if err != nil {
			t.Fatal(err)
		}
	}

	err = os.Symlink("nested/data.txt", filepath.Join(src, "link"))
	if err != nil {
		t.Fatal(err)
	}

	return src
}

// TestCopyTreeClonesTheWholeDirectoryAtOnce: cp -p carries the source directory's own mtime over and one clonefile(2) stamps a new one, which is how this test knows the whole-directory clone ran rather than the per-file fallback.
func TestCopyTreeClonesTheWholeDirectoryAtOnce(t *testing.T) {
	t.Parallel()

	src := cloneSource(t)
	old := time.Unix(1_000_000, 0)

	err := os.Chtimes(src, old, old)
	if err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "dst")

	err = os.Mkdir(dst, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = copyTree(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil || info.ModTime().Equal(old) {
		t.Errorf("dst = %v (%v), carrying the source's own mtime: cp copied file by file rather than one clone of the directory", info, err)
	}

	want, err := digestTree(src)
	if err != nil {
		t.Fatal(err)
	}

	got, err := digestTree(dst)
	if err != nil || got != want {
		t.Errorf("the clone digests %q (%v), want the source's %q", got, err, want)
	}
}

// TestCopyTreeFallsBackToCopyingFileByFile: any clonefile error leaves the destination to cp, which here merges into a directory a clone refuses to replace.
func TestCopyTreeFallsBackToCopyingFileByFile(t *testing.T) {
	t.Parallel()

	src := cloneSource(t)
	dst := t.TempDir()

	err := os.WriteFile(filepath.Join(dst, "already"), []byte("here"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = copyTree(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("copyTree: %v", err)
	}

	for _, name := range []string{"already", "run.sh", "nested/data.txt"} {
		_, statErr := os.Stat(filepath.Join(dst, name))
		if statErr != nil {
			t.Errorf("%s missing after the fallback copy: %v", name, statErr)
		}
	}

	info, err := os.Lstat(filepath.Join(dst, "link"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link after the fallback = %v (%v), want the symlink itself", info, err)
	}
}
