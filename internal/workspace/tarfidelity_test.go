package workspace

// The contract between internal/wire's tar codec and this package's digest.
//
// It lives here rather than next to the codec because digestTree is
// unexported, and it is the digest that decides whether a round-tripped tree
// is the same content — so the test that proves the codec preserves it belongs
// with the thing being preserved.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// treeShape is one arrangement of files that a tar codec can get wrong. Each
// entry is a bug some archiver has actually shipped.
type treeShape struct {
	name  string
	build func(t *testing.T, root string)
}

func treeShapes() []treeShape {
	return []treeShape{
		{"an executable script", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "run.sh", "#!/bin/sh\necho hi\n", 0o700)
		}},
		{"a plain file beside an executable one", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "run.sh", "#!/bin/sh\n", 0o700)
			write(t, root, "notes.txt", "plain\n", 0o600)
		}},
		{"a file with restrictive permissions", func(t *testing.T, root string) {
			t.Helper()
			// digestTree hashes only the executable bit, so 0600 and 0644 are
			// the same content. This pins that the codec does not accidentally
			// make them differ by carrying the rest of the mode.
			write(t, root, "secret", "shh\n", 0o600)
		}},
		{"an empty file", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "empty", "", 0o600)
		}},
		{"an empty directory", func(t *testing.T, root string) {
			t.Helper()
			// An absent path and an empty one must not collide, so the codec
			// has to emit a directory with no children of its own.
			mkdir(t, root, "empty-dir")
		}},
		{"nested directories", func(t *testing.T, root string) {
			t.Helper()
			mkdir(t, root, filepath.Join("a", "b", "c"))
			write(t, root, filepath.Join("a", "b", "c", "deep.txt"), "deep\n", 0o600)
		}},
		{"a relative symlink", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "target.txt", "target\n", 0o600)
			symlink(t, root, "target.txt", "link")
		}},
		{"a symlink to an absolute path outside the tree", func(t *testing.T, root string) {
			t.Helper()
			// digestTree hashes the link's own text and never follows it, so
			// this must round-trip verbatim rather than being resolved.
			symlink(t, root, "/etc/hosts", "escape")
		}},
		{"a dangling symlink", func(t *testing.T, root string) {
			t.Helper()
			symlink(t, root, "nowhere.txt", "dangling")
		}},
		{"a symlink pointing up out of the tree", func(t *testing.T, root string) {
			t.Helper()
			symlink(t, root, filepath.Join("..", "..", "elsewhere"), "up")
		}},
		{"a name with a space", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "two words.txt", "spaced\n", 0o600)
		}},
		{"a unicode name", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "café-☕.txt", "unicode\n", 0o600)
		}},
		{"a long path", func(t *testing.T, root string) {
			t.Helper()
			// Past USTAR's 100-byte name limit, which is why the codec pins
			// tar.FormatPAX rather than letting the writer choose.
			deep := filepath.Join("a-directory-with-a-fairly-long-name", "and-another-one-just-like-it",
				"and-a-third-for-good-measure", "still-going", "file.txt")
			mkdir(t, root, filepath.Dir(deep))
			write(t, root, deep, "deep\n", 0o600)
		}},
		{"a file containing NUL bytes", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "binary.bin", "before\x00\x00after", 0o600)
		}},
		{"a populated tree of every kind at once", func(t *testing.T, root string) {
			t.Helper()
			write(t, root, "run.sh", "#!/bin/sh\n", 0o700)
			write(t, root, "readme.md", "# hi\n", 0o600)
			mkdir(t, root, "empty-dir")
			mkdir(t, root, "sub")
			write(t, root, filepath.Join("sub", "nested.txt"), "nested\n", 0o600)
			symlink(t, root, filepath.Join("..", "readme.md"), filepath.Join("sub", "up-link"))
		}},
	}
}

// TestPackTreePreservesDigest is the gate on the whole remote-execution
// feature. Placement is deliberately absent from a step's cache key: a step
// tagged onto a worker is supposed to be the same work, cached the same way,
// as one that ran here. That holds only while a tree survives the wire with
// its digest intact — so this is the test that earns the right to leave tags:
// out of the hash, and a failure here means the step cache is silently wrong,
// not that a transfer is merely lossy.
func TestPackTreePreservesDigest(t *testing.T) {
	t.Parallel()

	for _, shape := range treeShapes() {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			shape.build(t, src)

			dst := t.TempDir()
			roundTrip(t, src, dst)

			want := mustDigest(t, src)
			if got := mustDigest(t, dst); got != want {
				t.Fatalf("digest changed across the round trip:\n  before %s\n  after  %s", want, got)
			}
		})
	}
}

// TestPackTreeDigestCanFail proves the round-trip test is not vacuous. Each
// mutation is a real bug a tar codec ships with, and each has to move the
// digest — a corruption the digest cannot see would mean the round trip is not
// obliged to preserve that property, which is worth knowing either way.
func TestPackTreeDigestCanFail(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		build   func(t *testing.T, root string)
		corrupt func(t *testing.T, root string)
	}{
		{
			name:  "losing the executable bit",
			build: func(t *testing.T, root string) { t.Helper(); write(t, root, "run.sh", "#!/bin/sh\n", 0o700) },
			corrupt: func(t *testing.T, root string) {
				t.Helper()

				err := os.Chmod(filepath.Join(root, "run.sh"), 0o600)
				if err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
		},
		{
			name: "resolving a symlink instead of carrying it",
			build: func(t *testing.T, root string) {
				t.Helper()
				write(t, root, "target.txt", "target\n", 0o600)
				symlink(t, root, "target.txt", "link")
			},
			corrupt: func(t *testing.T, root string) {
				t.Helper()

				err := os.Remove(filepath.Join(root, "link"))
				if err != nil {
					t.Fatalf("remove: %v", err)
				}

				write(t, root, "link", "target\n", 0o600)
			},
		},
		{
			name:  "dropping an empty directory",
			build: func(t *testing.T, root string) { t.Helper(); mkdir(t, root, "empty-dir") },
			corrupt: func(t *testing.T, root string) {
				t.Helper()

				err := os.Remove(filepath.Join(root, "empty-dir"))
				if err != nil {
					t.Fatalf("remove: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			tc.build(t, src)

			dst := t.TempDir()
			roundTrip(t, src, dst)
			tc.corrupt(t, dst)

			if mustDigest(t, src) == mustDigest(t, dst) {
				t.Fatal("the digest did not notice — this round-trip test cannot fail, so it proves nothing")
			}
		})
	}
}

// TestPackTreeRefusesUnsupportedEntries pins the refusal as a contract rather
// than an accident. Skipping a fifo would drop a path digestTree counts, which
// changes the tree's digest and poisons the step cache without saying so.
func TestPackTreeRefusesUnsupportedEntries(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, root, "ordinary.txt", "fine\n", 0o600)
	mkfifo(t, filepath.Join(root, "queue"))

	err := wire.PackTree(&bytes.Buffer{}, root)
	if err == nil {
		t.Fatal("PackTree succeeded on a tree containing a fifo, want a refusal naming it")
	}

	if !strings.Contains(err.Error(), "queue") {
		t.Errorf("error = %v, want it to name the offending entry", err)
	}
}

func roundTrip(t *testing.T, src, dst string) {
	t.Helper()

	var buf bytes.Buffer

	err := wire.PackTree(&buf, src)
	if err != nil {
		t.Fatalf("PackTree: %v", err)
	}

	err = wire.UnpackTree(&buf, dst)
	if err != nil {
		t.Fatalf("UnpackTree: %v", err)
	}
}

func write(t *testing.T, root, name, content string, mode os.FileMode) {
	t.Helper()

	path := filepath.Join(root, name)

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatalf("mkdir for %q: %v", name, err)
	}

	err = os.WriteFile(path, []byte(content), mode)
	if err != nil {
		t.Fatalf("write %q: %v", name, err)
	}

	// WriteFile's mode is masked by umask, and the executable bit is the one
	// thing the digest actually reads.
	err = os.Chmod(path, mode)
	if err != nil {
		t.Fatalf("chmod %q: %v", name, err)
	}
}

func mkdir(t *testing.T, root, name string) {
	t.Helper()

	err := os.MkdirAll(filepath.Join(root, name), 0o750)
	if err != nil {
		t.Fatalf("mkdir %q: %v", name, err)
	}
}

func symlink(t *testing.T, root, target, name string) {
	t.Helper()

	path := filepath.Join(root, name)

	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		t.Fatalf("mkdir for %q: %v", name, err)
	}

	err = os.Symlink(target, path)
	if err != nil {
		t.Fatalf("symlink %q -> %q: %v", name, target, err)
	}
}
