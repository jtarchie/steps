package wire

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestPackTreeAsIsPackPathsOfTheTreeUnderThatName pins the equality the
// worker's cache depends on: a tree packed AS a name produces the same bytes
// as PackPaths over a directory holding that tree under the name. The shim
// files the former's digest; the orchestrator offers the latter's.
func TestPackTreeAsIsPackPathsOfTheTreeUnderThatName(t *testing.T) {
	t.Parallel()

	tree := filepath.Join(t.TempDir(), "work")
	writeAt(t, filepath.Join(tree, "top.txt"), "one\n")
	writeAt(t, filepath.Join(tree, "nested", "deep.txt"), "two\n")

	err := os.Chmod(tree, 0o700) //nolint:gosec // a directory this test made, narrowed deliberately so the mode is part of what is compared
	if err != nil {
		t.Fatal(err)
	}

	var as bytes.Buffer

	err = PackTreeAs(&as, tree, "src")
	if err != nil {
		t.Fatalf("PackTreeAs: %v", err)
	}

	parent := t.TempDir()

	err = os.Rename(tree, filepath.Join(parent, "src"))
	if err != nil {
		t.Fatal(err)
	}

	var paths bytes.Buffer

	err = PackPaths(&paths, parent, []string{"src"})
	if err != nil {
		t.Fatalf("PackPaths: %v", err)
	}

	if !bytes.Equal(as.Bytes(), paths.Bytes()) {
		t.Error("PackTreeAs and PackPaths disagree on the same tree under the same name — the worker files one digest and the orchestrator offers another")
	}
}

// TestPackTreeAsRefusesAnUnsafeName: the name comes from the peer, and a
// fetch-all named "../x" would file — and later extract — outside the tree.
func TestPackTreeAsRefusesAnUnsafeName(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	writeAt(t, filepath.Join(tree, "f"), "x")

	for _, name := range []string{"../x", "/abs", ".", ""} {
		err := PackTreeAs(&bytes.Buffer{}, tree, name)
		if err == nil {
			t.Errorf("PackTreeAs(%q) succeeded, want a refusal", name)
		}
	}
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(path, []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}
