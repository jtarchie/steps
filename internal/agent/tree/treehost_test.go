package tree

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Resolve checks a path and the tool opens it afterwards, by name; a run_shell racing between the two can swap a checked file for a link out of the workspace. Opening is where that has to be refused, so these hand the open a path Resolve would have accepted before the swap.
func TestHostTreeOpensNothingALinkSwappedInAfterResolveReachesOutside(t *testing.T) {
	t.Parallel()

	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")

	err := os.WriteFile(secret, []byte("SENTINEL"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "x")

	err = os.Symlink(secret, link)
	if err != nil {
		t.Fatal(err)
	}

	host := Host{Dir: dir}

	data, err := host.ReadBytes(t.Context(), link, 1<<20)
	if err == nil {
		t.Errorf("ReadBytes through a link out of the workspace read %q", data)
	}

	f, err := host.OpenRange(t.Context(), link, 0)
	if err == nil {
		_ = f.Close()

		t.Error("OpenRange through a link out of the workspace opened it")
	}

	err = host.WriteFile(t.Context(), link, []byte("OVERWRITTEN"), false)
	if err == nil {
		t.Error("WriteFile through a link out of the workspace succeeded")
	}

	got, err := os.ReadFile(secret) //nolint:gosec // the test's own temp file
	if err != nil || string(got) != "SENTINEL" {
		t.Errorf("the file outside the workspace is now %q (%v)", got, err)
	}

	err = os.Symlink(outside, filepath.Join(dir, "d"))
	if err != nil {
		t.Fatal(err)
	}

	entries, err := host.ListDir(t.Context(), filepath.Join(dir, "d"))
	if err == nil {
		t.Errorf("ListDir through a link out of the workspace listed %+v", entries)
	}
}

func TestHostTreeStillFollowsALinkThatStaysInside(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "real"), []byte("inside"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink("real", filepath.Join(dir, "alias"))
	if err != nil {
		t.Fatal(err)
	}

	data, err := Host{Dir: dir}.ReadBytes(t.Context(), filepath.Join(dir, "alias"), 1<<20)
	if err != nil || string(data) != "inside" {
		t.Errorf("ReadBytes(alias) = %q, %v", data, err)
	}
}

// The walk skips a link it sees, but a regular file it saw can be a link by the time it is opened; the opening is what must refuse it.
func TestHostSearchOpensNoFileThroughALinkOutOfTheWorkspace(t *testing.T) {
	t.Parallel()

	dir, outside := t.TempDir(), t.TempDir()
	secret := filepath.Join(outside, "secret")

	err := os.WriteFile(secret, []byte("SENTINEL\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(dir, "x")

	err = os.Symlink(secret, link)
	if err != nil {
		t.Fatal(err)
	}

	opts := SearchOpts{Re: regexp.MustCompile("SENTINEL"), Mode: "content", HeadLimit: 10, open: func(path string) (*os.File, error) {
		return Host{Dir: dir}.open(path, os.O_RDONLY, 0)
	}}

	_, count := scanFileMatches(link, "x", opts)
	if count != 0 {
		t.Errorf("the scan read %d matching lines through a link out of the workspace", count)
	}
}
