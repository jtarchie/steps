package venue

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// A listing decides what is removed at home, so one naming a path outside the tree is refused rather than followed.
func TestParseListingsRefusesEscapes(t *testing.T) {
	t.Parallel()

	got, err := parseListings("D.\x00Ea\x00E.hidden\x00D./sub\x00Eb\x00")
	if err != nil || !got["."]["a"] || !got["."][".hidden"] || !got["sub"]["b"] {
		t.Fatalf("parseListings = %v, %v; want . holding a and .hidden, sub holding b", got, err)
	}

	for _, bad := range []string{"D../up\x00", "D/etc\x00", "D.\x00E..\x00", "D.\x00Ea/b\x00", "Eorphan\x00", "Xjunk\x00"} {
		_, err := parseListings(bad)
		if !errors.Is(err, errBadListing) {
			t.Errorf("parseListings(%q) = %v, want errBadListing", bad, err)
		}
	}
}

// A directory that is a symlink at home is replaced, never followed: pruning through it would delete whatever it points at.
func TestApplyChangesNeverFollowsALinkOutOfTheOutput(t *testing.T) {
	t.Parallel()

	outside, home, staging := t.TempDir(), t.TempDir(), t.TempDir()

	err := os.WriteFile(filepath.Join(outside, "precious"), []byte("keep\n"), 0o600)
	if err == nil {
		err = os.Symlink(outside, filepath.Join(home, "sub"))
	}

	if err == nil {
		err = os.MkdirAll(filepath.Join(staging, "sub"), 0o700)
	}

	if err == nil {
		err = os.WriteFile(filepath.Join(staging, "sub", "new.txt"), []byte("new\n"), 0o600)
	}

	if err != nil {
		t.Fatal(err)
	}

	err = applyChanges(staging, home, map[string]map[string]bool{".": {"sub": true}, "sub": {"new.txt": true}})
	if err != nil {
		t.Fatalf("applyChanges: %v", err)
	}

	_, err = os.Stat(filepath.Join(outside, "precious"))
	if err != nil {
		t.Errorf("a file outside the output was removed through a symlink: %v", err)
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

	err = applyChanges(staging, home, map[string]map[string]bool{".": {"a": true, "x": true}})
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

	err = applyChanges(staging, home, map[string]map[string]bool{".": {"a": true, "x": true}, "a": {"up": true}})
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
		{Typeflag: tar.TypeReg, Name: "./kept.txt", Mode: 0o644, Size: 3},
	})

	var out bytes.Buffer

	err := dropWhiteouts(in, &out)
	if err != nil {
		t.Fatalf("dropWhiteouts: %v", err)
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

	for _, gone := range []string{"a", "b", "sub/c"} {
		_, err = root.Lstat(gone)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s = %v, want no entry: it stands for a deletion", gone, err)
		}
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
