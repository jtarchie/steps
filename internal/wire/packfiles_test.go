package wire

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// git lists a tracked file deleted from the checkout, and a listed name the disk lost is not a reason to refuse the rest.
func TestPackFilesSkipsANameThatIsGone(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	writeAt(t, filepath.Join(tree, "nested", "kept.txt"), "kept\n")
	writeAt(t, filepath.Join(tree, "unlisted.txt"), "left out\n")

	var packed bytes.Buffer

	err := PackFiles(&packed, tree, []string{"gone.txt", "nested/kept.txt"})
	if err != nil {
		t.Fatalf("PackFiles: %v", err)
	}

	out := t.TempDir()

	err = UnpackFetchedTree(&packed, out, UnpackOptions{})
	if err != nil {
		t.Fatalf("UnpackFetchedTree: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(out, "nested", "kept.txt")) //nolint:gosec // a t.TempDir() path
	if err != nil || string(got) != "kept\n" {
		t.Errorf("nested/kept.txt = %q, %v", got, err)
	}

	_, err = os.Stat(filepath.Join(out, "unlisted.txt"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a name not listed was packed: %v", err)
	}
}

func TestPackFilesRefusesANameOutsideTheTree(t *testing.T) {
	t.Parallel()

	err := PackFiles(&bytes.Buffer{}, t.TempDir(), []string{"../escape"})
	if !errors.Is(err, ErrUnsafePath) {
		t.Errorf("PackFiles(../escape) = %v, want ErrUnsafePath", err)
	}
}
