package venue

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// An opaque directory is removed whole at home, so a name outside the tree, or the tree itself, is refused rather than removed.
func TestParseOpaqueRefusesEscapes(t *testing.T) {
	t.Parallel()

	got, err := parseOpaque("./a\x00./sub/b c\x00")
	if err != nil || !slices.Equal(got, []string{"a", "sub/b c"}) {
		t.Fatalf("parseOpaque = %v, %v; want [a sub/b c]", got, err)
	}

	for _, bad := range []string{"../up\x00", "/etc\x00", ".\x00", "./a/../..\x00"} {
		_, err := parseOpaque(bad)
		if !errors.Is(err, errBadDeletion) {
			t.Errorf("parseOpaque(%q) = %v, want errBadDeletion", bad, err)
		}
	}
}

// Deletions and a recreated directory are applied before staging lands: a whiteout's path goes, an opaque directory loses what staging does not hold, and nothing unnamed is touched.
func TestApplyChangesRemovesOnlyWhatIsGone(t *testing.T) {
	t.Parallel()

	home, staging := t.TempDir(), t.TempDir()

	for _, name := range []string{"gone.txt", "kept.txt", "dir/old", "dir/sub/deep", "gonedir/x"} {
		mustWrite(t, filepath.Join(home, name), "home\n")
	}

	mustWrite(t, filepath.Join(staging, "dir", "new"), "new\n")

	err := applyChanges(staging, home, []string{"gone.txt", "gonedir", "missing/parent", "kept.txt/under", "dir"})
	if err != nil {
		t.Fatalf("applyChanges: %v", err)
	}

	for _, gone := range []string{"gone.txt", "gonedir", "dir/old", "dir/sub"} {
		_, err = os.Lstat(filepath.Join(home, gone))
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s = %v, want it removed", gone, err)
		}
	}

	for _, kept := range []string{"kept.txt", "dir/new"} {
		_, err = os.Lstat(filepath.Join(home, kept))
		if err != nil {
			t.Errorf("%s = %v, want it at home", kept, err)
		}
	}
}

// A directory that is a symlink at home is replaced, never followed: removing through it would delete whatever it points at.
func TestApplyChangesNeverFollowsALinkOutOfTheOutput(t *testing.T) {
	t.Parallel()

	outside, home, staging := t.TempDir(), t.TempDir(), t.TempDir()

	mustWrite(t, filepath.Join(outside, "precious"), "keep\n")
	mustWrite(t, filepath.Join(staging, "sub", "new.txt"), "new\n")

	err := os.Symlink(outside, filepath.Join(home, "sub"))
	if err != nil {
		t.Fatal(err)
	}

	err = applyChanges(staging, home, []string{"sub/precious"})
	if err == nil {
		t.Errorf("applyChanges removed a deletion through a link out of the output, want it refused")
	}

	_, err = os.Stat(filepath.Join(outside, "precious"))
	if err != nil {
		t.Fatalf("a file outside the output was removed through a symlink: %v", err)
	}

	err = applyChanges(staging, home, []string{"sub"})
	if err != nil {
		t.Fatalf("applyChanges: %v", err)
	}

	_, err = os.Stat(filepath.Join(outside, "precious"))
	if err != nil {
		t.Errorf("a recreated directory that is a link at home was followed: %v", err)
	}

	info, err := os.Lstat(filepath.Join(home, "sub"))
	if err != nil || !info.IsDir() {
		t.Errorf("home/sub = %v, %v; want the staged directory in place of the link", info, err)
	}
}

// Staging holds only the changes, so a new link chaining through one already at home (a/up -> ..) looks dangling to the fetch's own checks; it is checked again once it stands in the home tree, and removed.
func TestApplyChangesRefusesALinkEscapingThroughOneAlreadyHome(t *testing.T) {
	t.Parallel()

	home, staging := t.TempDir(), t.TempDir()

	err := os.MkdirAll(filepath.Join(home, "a"), 0o700)
	if err == nil {
		err = os.Symlink("..", filepath.Join(home, "a", "up"))
	}

	if err == nil {
		err = os.Symlink("a/up/a/up/../../etc", filepath.Join(staging, "x"))
	}

	if err != nil {
		t.Fatal(err)
	}

	err = applyChanges(staging, home, nil)
	if !errors.Is(err, wire.ErrUnsafeLink) {
		t.Fatalf("applyChanges = %v, want ErrUnsafeLink", err)
	}

	_, err = os.Lstat(filepath.Join(home, "x"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home/x = %v, want the escaping link removed", err)
	}
}

// The other direction: the step turns a/up into a link to .., and a link it never touched, which went nowhere while a/up was a directory, now climbs out.
func TestApplyChangesRefusesAnUntouchedLinkAChangeMadeEscape(t *testing.T) {
	t.Parallel()

	home, staging := t.TempDir(), t.TempDir()

	err := os.MkdirAll(filepath.Join(home, "a", "up"), 0o700)
	if err == nil {
		err = os.Symlink("a/up/a/up/../../etc", filepath.Join(home, "x"))
	}

	if err == nil {
		err = os.MkdirAll(filepath.Join(staging, "a"), 0o700)
	}

	if err == nil {
		err = os.Symlink("..", filepath.Join(staging, "a", "up"))
	}

	if err != nil {
		t.Fatal(err)
	}

	err = applyChanges(staging, home, nil)
	if !errors.Is(err, wire.ErrUnsafeLink) {
		t.Fatalf("applyChanges = %v, want ErrUnsafeLink", err)
	}

	_, err = os.Lstat(filepath.Join(home, "x"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("home/x = %v, want the escaping link removed", err)
	}
}

// An overlay on Linux 5.8+ hard-links every whiteout after the first to one inode, so a step that deleted two files sent a device and a hard link to it; the link was kept and the unpack refused it, failing the step (a work/prs.yml bot run that merged master's deletions).
func TestDropWhiteoutsDropsLinksToAWhiteout(t *testing.T) {
	t.Parallel()

	in := tarOf(t, []*tar.Header{
		{Typeflag: tar.TypeDir, Name: "./", Mode: 0o755},
		{Typeflag: tar.TypeChar, Name: "./b"},
		{Typeflag: tar.TypeLink, Name: "./a", Linkname: "./b"},
		{Typeflag: tar.TypeLink, Name: "sub/c", Linkname: "b"},
		{Typeflag: tar.TypeChar, Name: "./null", Devmajor: 1, Devminor: 3},
		{Typeflag: tar.TypeReg, Name: "./kept.txt", Mode: 0o644, Size: 3},
	})

	var out bytes.Buffer

	deleted, err := dropWhiteouts(in, &out, deletionsMaxBytes)
	if err != nil {
		t.Fatalf("dropWhiteouts: %v", err)
	}

	if !slices.Equal(deleted, []string{"b", "a", "sub/c"}) {
		t.Errorf("deleted = %v, want [b a sub/c]: a device that is not 0:0 is no whiteout", deleted)
	}

	staging := t.TempDir()

	err = wire.UnpackFetchedTree(&out, staging, wire.UnpackOptions{})
	if err != nil {
		t.Fatalf("UnpackFetchedTree after dropWhiteouts: %v", err)
	}

	root, err := os.OpenRoot(staging)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = root.Close() }()

	got, err := root.ReadFile("kept.txt")
	if err != nil || string(got) != "hi\n" {
		t.Errorf("kept.txt = %q, %v; want the regular file through", got, err)
	}

	for _, gone := range []string{"a", "b", "sub/c", "null"} {
		_, err = root.Lstat(gone)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s = %v, want no entry: it stands for a deletion", gone, err)
		}
	}
}

// A whiteout names what is removed at home, so one outside the tree is refused, and more than the bound fails the fetch rather than removing only some.
func TestDropWhiteoutsRefusesEscapesAndTooMany(t *testing.T) {
	t.Parallel()

	_, err := dropWhiteouts(tarOf(t, []*tar.Header{{Typeflag: tar.TypeChar, Name: "../up"}}), io.Discard, deletionsMaxBytes)
	if !errors.Is(err, errBadDeletion) {
		t.Errorf("an escaping whiteout = %v, want errBadDeletion", err)
	}

	many := tarOf(t, []*tar.Header{{Typeflag: tar.TypeChar, Name: "aaaa"}, {Typeflag: tar.TypeLink, Name: "bbbb", Linkname: "aaaa"}})

	_, err = dropWhiteouts(many, io.Discard, 7)
	if !errors.Is(err, errTooManyDeletions) {
		t.Errorf("deletions past the bound = %v, want errTooManyDeletions", err)
	}
}

// tarOf writes headers as a tar stream, each regular file holding "hi\n".
func tarOf(t *testing.T, headers []*tar.Header) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer

	writer := tar.NewWriter(&buf)

	for _, header := range headers {
		err := writer.WriteHeader(header)
		if err == nil && header.Size > 0 {
			_, err = writer.Write([]byte("hi\n"))
		}

		if err != nil {
			t.Fatal(err)
		}
	}

	err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	return &buf
}
