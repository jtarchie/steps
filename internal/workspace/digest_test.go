package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTree lays out a directory from a path -> contents map and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		path := filepath.Join(root, name)

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func mustDigest(t *testing.T, root string) string {
	t.Helper()

	digest, err := digestTree(root)
	if err != nil {
		t.Fatal(err)
	}

	return digest
}

// TestDigestTreeIgnoresWhatIsNotContent is what makes the step cache hit at
// all: two checkouts of the same content, made at different times into
// different directories, must be the same key.
func TestDigestTreeIgnoresWhatIsNotContent(t *testing.T) {
	t.Parallel()

	files := map[string]string{"a.txt": "alpha", "nested/b.txt": "beta"}

	first := writeTree(t, files)

	// A different directory, written later, with different mtimes.
	time.Sleep(10 * time.Millisecond)

	second := writeTree(t, files)

	if mustDigest(t, first) != mustDigest(t, second) {
		t.Error("the same content in two directories digested differently — the cache would never hit")
	}
}

// TestDigestTreeSeesEveryKindOfChange is the other half: anything a later step
// could read differently has to change the key.
func TestDigestTreeSeesEveryKindOfChange(t *testing.T) {
	t.Parallel()

	base := map[string]string{"a.txt": "alpha", "nested/b.txt": "beta"}
	baseline := mustDigest(t, writeTree(t, base))

	for name, files := range map[string]map[string]string{
		"changed content": {"a.txt": "ALPHA", "nested/b.txt": "beta"},
		"added file":      {"a.txt": "alpha", "nested/b.txt": "beta", "c.txt": "gamma"},
		"removed file":    {"a.txt": "alpha"},
		"renamed file":    {"a.txt": "alpha", "nested/renamed.txt": "beta"},
		"moved content":   {"a.txt": "alphabeta", "nested/b.txt": ""},
	} {
		if mustDigest(t, writeTree(t, files)) == baseline {
			t.Errorf("%s digested the same as the baseline — a step would reuse a stale result", name)
		}
	}
}

// TestDigestTreeSeesAnEmptyFile pins the case a naive "hash the bytes"
// implementation collapses: a file with no content is not the same tree as no
// file at all.
func TestDigestTreeSeesAnEmptyFile(t *testing.T) {
	t.Parallel()

	withFile := mustDigest(t, writeTree(t, map[string]string{"a.txt": "alpha", "empty.txt": ""}))
	without := mustDigest(t, writeTree(t, map[string]string{"a.txt": "alpha"}))

	if withFile == without {
		t.Error("an empty file digested the same as no file")
	}
}

// TestDigestTreeSeesTheExecutableBit: a script that can no longer be run is
// not the same input, even though its bytes are.
func TestDigestTreeSeesTheExecutableBit(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{"run.sh": "echo hi"})
	before := mustDigest(t, root)

	err := os.Chmod(filepath.Join(root, "run.sh"), 0o700) //nolint:gosec // making a file executable is the thing under test
	if err != nil {
		t.Fatal(err)
	}

	if mustDigest(t, root) == before {
		t.Error("making a file executable did not change its digest")
	}
}

// TestDigestTreeHashesASymlinkAsItsOwnText: following the link would let a
// path outside the artifact — /etc, a home directory — decide a cache key.
func TestDigestTreeHashesASymlinkAsItsOwnText(t *testing.T) {
	t.Parallel()

	root := writeTree(t, map[string]string{"a.txt": "alpha"})
	target := filepath.Join(t.TempDir(), "outside.txt")

	err := os.WriteFile(target, []byte("host state"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(target, filepath.Join(root, "link"))
	if err != nil {
		t.Fatal(err)
	}

	digest := mustDigest(t, root)

	// Change what the link points AT. The link's own text is unchanged, so the
	// digest must be too — the alternative is a key that depends on a file the
	// pipeline never declared.
	err = os.WriteFile(target, []byte("different host state"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if mustDigest(t, root) != digest {
		t.Error("the digest followed a symlink out of the artifact")
	}
}

// TestDigestTreeIsNotAmbiguous is why every variable-length field is
// length-prefixed: no arrangement of names and bytes may produce another
// tree's stream.
func TestDigestTreeIsNotAmbiguous(t *testing.T) {
	t.Parallel()

	first := mustDigest(t, writeTree(t, map[string]string{"ab": "c", "d": "e"}))
	second := mustDigest(t, writeTree(t, map[string]string{"a": "bc", "d": "e"}))

	if first == second {
		t.Error("two different trees produced the same digest stream")
	}
}
