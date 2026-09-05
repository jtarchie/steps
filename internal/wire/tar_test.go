package wire

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestPackPathsRefusesAnEscapingName is the read-side trust boundary. The
// names PackPaths is given arrive from the PEER — the shim tars whatever a
// FrameFetch asked for — so an unvalidated name walked a tree outside the work
// directory and shipped it straight back as data frames. unpackName cannot
// cover it: that guard runs on the orchestrator, and whoever sent the frame is
// reading the raw stream.
func TestPackPathsRefusesAnEscapingName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	outside := filepath.Join(filepath.Dir(root), "secrets-"+filepath.Base(root))

	err := os.MkdirAll(outside, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	// The marker stands in for whatever is outside the tree; what the test
	// asserts is that no byte of it reaches the stream.
	marker := "bytes-from-outside-the-tree"

	err = os.WriteFile(filepath.Join(outside, "private"), []byte(marker), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"../" + filepath.Base(outside),
		"a/../../" + filepath.Base(outside),
		outside,
		"..",
		".",
		"",
	} {
		buf := new(bytes.Buffer)

		packErr := PackPaths(buf, root, []string{name})
		if packErr == nil {
			t.Errorf("PackPaths accepted %q, want a refusal", name)
		}

		if strings.Contains(buf.String(), marker) {
			t.Fatalf("PackPaths shipped a file from outside the tree for %q", name)
		}
	}
}

// TestPackPathsRefusesASymlinkedName is the half of the read-side boundary the
// lexical check cannot see.
//
// The names PackPaths is given arrive from the PEER, and so does the TREE: the
// codec round-trips a symlink with its target unvalidated, so an upload can
// plant one and the fetch that follows can walk through it. filepath.WalkDir
// Lstats only what it DISCOVERS — a symlink in the argument's own path is
// resolved by the kernel before the walk starts — so "esc/private" behind
// `esc -> <outside>` shipped a file from outside the work directory while
// every ".." check passed.
func TestPackPathsRefusesASymlinkedName(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	outside := filepath.Join(filepath.Dir(root), "secrets-"+filepath.Base(root))

	err := os.MkdirAll(outside, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(outside) })

	marker := "bytes-from-behind-a-symlink"

	err = os.WriteFile(filepath.Join(outside, "private"), []byte(marker), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// The link a step's own tree can legitimately contain, or that an upload
	// can plant.
	err = os.Symlink(outside, filepath.Join(root, "esc"))
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"esc/private", "esc"} {
		buf := new(bytes.Buffer)

		packErr := PackPaths(buf, root, []string{name})

		if strings.Contains(buf.String(), marker) {
			t.Fatalf("PackPaths shipped a file from outside the tree for %q", name)
		}

		if packErr == nil {
			t.Errorf("PackPaths accepted %q, want a refusal", name)
		}
	}

	// And an ordinary name still packs: the guard must not refuse the tree it
	// exists to bound.
	err = os.WriteFile(filepath.Join(root, "kept.txt"), []byte("inside"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	buf := new(bytes.Buffer)

	err = PackPaths(buf, root, []string{"kept.txt"})
	if err != nil {
		t.Fatalf("PackPaths refused a name inside the tree: %v", err)
	}

	if !strings.Contains(buf.String(), "inside") {
		t.Error("PackPaths shipped nothing for a name inside the tree")
	}
}

// TestPackPathsRefusesANameItCannotResolve is the other half of the same
// guard: the lexical check passed, the resolution did not happen, and the
// answer must not be "accepted".
//
// withinRoot climbs to the nearest resolvable ancestor because a named output
// that was never produced is deliberately tolerated — fs.ErrNotExist, and
// only that. Every other reason a probe fails (EACCES on the link's own
// target, ELOOP on a cycle, a stale mount) is a path this end cannot vouch
// for, and climbing past it reaches the root, which resolves by construction,
// and returns nil for a name nobody checked. Measured on one link a single
// chmod apart: a 0700 target refused, a 0000 target accepted.
func TestPackPathsRefusesANameItCannotResolve(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, where a 0000 directory is still traversable and the resolution never fails")
	}

	root := t.TempDir()
	outside := t.TempDir()

	err := os.MkdirAll(filepath.Join(outside, "inner"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	// One level deeper than the link's own target, so what fails is the
	// resolution of the LINK — not of a name below an unreadable directory,
	// which the loop would still refuse on the ancestor it can resolve.
	err = os.Symlink(filepath.Join(outside, "inner"), filepath.Join(root, "esc"))
	if err != nil {
		t.Fatal(err)
	}

	// Restored before TempDir's own cleanup, which cannot remove a directory
	// it may not traverse. Registered after it, so it runs first.
	t.Cleanup(func() { _ = os.Chmod(outside, 0o700) }) //nolint:gosec // a directory, restored so TempDir can traverse it

	err = os.Chmod(outside, 0o000)
	if err != nil {
		t.Fatal(err)
	}

	buf := new(bytes.Buffer)

	err = PackPaths(buf, root, []string{"esc"})
	if !errors.Is(err, ErrUnsafePath) {
		t.Errorf("PackPaths(%q) = %v, want a refusal: the guard could not resolve the name and answered as though it had",
			"esc", err)
	}
}

// TestPackPathsAcceptsARelativeRoot pins the guard against the one root
// spelling that has no prefix to be.
//
// withinRoot decides "inside the tree" by resolving both the root and the
// member and comparing prefixes. filepath.EvalSymlinks(".") answers "." while
// the member under it resolves to a bare "out", which is neither equal to "."
// nor prefixed by "./" — so every name inside the tree was reported as an
// escape, and PackPaths wrote nothing while naming a security failure it had
// not observed. A guard that cannot tell inside from outside is worse than no
// guard, because it is believed.
func TestPackPathsAcceptsARelativeRoot(t *testing.T) {
	root := t.TempDir()

	err := os.MkdirAll(filepath.Join(root, "out"), 0o700)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(root, "out", "a.txt"), []byte("hi\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(root)

	for _, spelling := range []string{".", "./", ""} {
		var buffer bytes.Buffer

		err := PackPaths(&buffer, spelling, []string{"out"})
		if err != nil {
			t.Errorf("PackPaths(root=%q): %v", spelling, err)

			continue
		}

		if buffer.Len() == 0 {
			t.Errorf("PackPaths(root=%q) wrote nothing", spelling)
		}
	}

	// The guard still has to fire, or "fixed" would mean "disarmed". A symlink
	// out of the tree is what it exists to refuse.
	err = os.Symlink(filepath.Dir(root), filepath.Join(root, "escape"))
	if err != nil {
		t.Fatal(err)
	}

	err = PackPaths(io.Discard, ".", []string{"escape"})
	if !errors.Is(err, ErrUnsafePath) {
		t.Errorf("PackPaths(escape) = %v, want ErrUnsafePath", err)
	}
}

// TestUnpackFetchedRefusesASymlinkOutOfTheTree: a worker answering a fetch
// could plant a link to a path on THIS machine, which the copy into a later
// step and that step's own reading would follow. Refused like an escaping
// name — for a FETCHED tree; a tree sent out keeps its links verbatim, which
// workspace's fidelity tests pin.
func TestUnpackFetchedRefusesASymlinkOutOfTheTree(t *testing.T) {
	t.Parallel()

	for name, target := range map[string]string{
		"absolute":    "/etc/passwd",
		"climbs out":  "../../outside",
		"climbs deep": "sub/../../..",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := UnpackFetchedTree(symlinkArchive(t, "dir/link", target), t.TempDir())
			if !errors.Is(err, ErrUnsafeLink) {
				t.Errorf("UnpackFetchedTree(link -> %q) = %v, want ErrUnsafeLink", target, err)
			}

			// The lenient unpack still takes it: a tree going OUT is verbatim.
			err = UnpackTree(symlinkArchive(t, "dir/link", target), t.TempDir())
			if err != nil {
				t.Errorf("UnpackTree(link -> %q) = %v, want the verbatim round trip", target, err)
			}
		})
	}

	// And one that stays inside, so the guard does not refuse the ordinary
	// relative link a checkout is full of.
	err := UnpackFetchedTree(symlinkArchive(t, "dir/link", "../sibling"), t.TempDir())
	if err != nil {
		t.Errorf("UnpackFetchedTree(link -> ../sibling) = %v, want an inside link accepted", err)
	}
}

// symlinkArchive is a tar holding one symlink.
func symlinkArchive(t *testing.T, name, target string) io.Reader {
	t.Helper()

	buf := new(bytes.Buffer)
	w := tar.NewWriter(buf)

	err := w.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target})
	if err != nil {
		t.Fatal(err)
	}

	err = w.Close()
	if err != nil {
		t.Fatal(err)
	}

	return buf
}
