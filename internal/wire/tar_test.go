package wire

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUnpackTreeRefusesEscapes covers the half of the trust boundary that
// faces a worker. internal/workspace already refuses a step that replaces its
// own output with a symlink on the way out; an archive arriving from somewhere
// else has to be refused the same way on the way in, or the protection sits on
// the wrong side of the wire.
func TestUnpackTreeRefusesEscapes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		archive func(w *tar.Writer)
	}{
		{"a relative path climbing out", func(w *tar.Writer) {
			writeRegular(w, "../escaped.txt")
		}},
		{"a path climbing out from deeper in", func(w *tar.Writer) {
			writeRegular(w, "sub/../../escaped.txt")
		}},
		{"an absolute path", func(w *tar.Writer) {
			writeRegular(w, "/tmp/escaped.txt")
		}},
		{"a file written through a symlink the same archive planted", func(w *tar.Writer) {
			// The interesting one: each entry is individually innocent. Only
			// the pair escapes, which is why the refusal has to be at the
			// filesystem boundary (os.Root) and not a name check alone.
			_ = w.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "link", Linkname: "/tmp", Format: tar.FormatPAX})
			writeRegular(w, "link/escaped.txt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer

			writer := tar.NewWriter(&buf)
			tc.archive(writer)
			_ = writer.Close()

			root := t.TempDir()

			err := UnpackTree(&buf, root)
			if err == nil {
				t.Fatal("UnpackTree accepted an archive that writes outside the tree")
			}

			_, statErr := os.Stat("/tmp/escaped.txt")
			if statErr == nil {
				t.Fatal("the archive actually wrote outside the tree")
			}
		})
	}
}

// TestUnpackTreeRefusesUnsupportedEntries is the inbound twin of PackTree's
// refusal: an archive from elsewhere must not be able to materialize a device
// node inside a step's workspace.
func TestUnpackTreeRefusesUnsupportedEntries(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)
	_ = writer.WriteHeader(&tar.Header{Typeflag: tar.TypeFifo, Name: "queue", Format: tar.FormatPAX})
	_ = writer.Close()

	err := UnpackTree(&buf, t.TempDir())
	if !errors.Is(err, ErrUnsupportedEntry) {
		t.Fatalf("UnpackTree error = %v, want ErrUnsupportedEntry", err)
	}
}

// TestPackTreeIsReproducible pins that the same tree packs to the same bytes.
// It is what makes the stream comparable at all — and it only holds because
// the codec normalizes away mtimes and ownership, which vary between two
// machines holding identical content.
func TestPackTreeIsReproducible(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	for name, content := range map[string]string{"a.txt": "alpha", "sub/b.txt": "beta"} {
		path := filepath.Join(root, filepath.FromSlash(name))

		err := os.MkdirAll(filepath.Dir(path), 0o750)
		if err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		err = os.WriteFile(path, []byte(content), 0o600)
		if err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	// Backdate one file: an mtime the codec carried would show up here as
	// different bytes between the two packs.
	past := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)

	err := os.Chtimes(filepath.Join(root, "a.txt"), past, past)
	if err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	var first, second bytes.Buffer

	err = PackTree(&first, root)
	if err != nil {
		t.Fatalf("PackTree: %v", err)
	}

	err = PackTree(&second, root)
	if err != nil {
		t.Fatalf("PackTree: %v", err)
	}

	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("packing the same tree twice produced different bytes")
	}
}

// writeRegular puts one ordinary file in an archive. The content is fixed
// because none of these tests care what lands, only whether it lands somewhere
// it should not.
func writeRegular(w *tar.Writer, name string) {
	const content = "nope"

	_ = w.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     0o600,
		Size:     int64(len(content)),
		Format:   tar.FormatPAX,
	})
	_, _ = w.Write([]byte(content))
}

// TestTreeRoundTripPreservesPermissions pins that a mode survives the wire.
//
// digestTree records only whether a file is executable, so nothing in the
// cache-key machinery can observe this — which is exactly why it needs its own
// test. Collapsing every file to 0644 was reproducible and wrong: a step that
// fetched a 0600 deploy key handed the worker a world-readable one, and ssh
// refuses to use it. The widening is the bug; the reproducibility it was
// protecting is unaffected, because the mode is read from the file rather than
// from whichever umask created it.
func TestTreeRoundTripPreservesPermissions(t *testing.T) {
	t.Parallel()

	modes := []os.FileMode{0o600, 0o400, 0o640, 0o700, 0o755, 0o644}

	src := t.TempDir()

	for _, mode := range modes {
		name := fmt.Sprintf("mode-%04o", mode)

		err := os.WriteFile(filepath.Join(src, name), []byte("content\n"), mode)
		if err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}

		err = os.Chmod(filepath.Join(src, name), mode)
		if err != nil {
			t.Fatalf("chmod %s: %v", name, err)
		}
	}

	var buf bytes.Buffer

	err := PackTree(&buf, src)
	if err != nil {
		t.Fatalf("PackTree: %v", err)
	}

	dst := t.TempDir()

	err = UnpackTree(&buf, dst)
	if err != nil {
		t.Fatalf("UnpackTree: %v", err)
	}

	for _, mode := range modes {
		name := fmt.Sprintf("mode-%04o", mode)

		info, err := os.Stat(filepath.Join(dst, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}

		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s came back %04o, want %04o", name, got, mode)
		}
	}
}
