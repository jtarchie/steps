package venue

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
