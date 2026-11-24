package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func grantsByPath(t *testing.T, writable, hidden []string) map[string]landlockAccess {
	t.Helper()

	grants, err := landlockGrants(writable, hidden)
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]landlockAccess{}

	for _, grant := range grants {
		if prior, dup := out[grant.path]; dup && prior != grant.access {
			t.Fatalf("%s granted both %d and %d", grant.path, prior, grant.access)
		}

		out[grant.path] = grant.access
	}

	return out
}

// Landlock only allows, and a read grant covers its whole tree, so the credential stays hidden only if no rule names it or anything above it with more than a listing.
func TestLandlockGrantsReadEverythingButTheHiddenPath(t *testing.T) {
	t.Parallel()

	home := fakeHome(t)
	grants := grantsByPath(t, nil, []string{home + "/.ssh", home + "/.config/gh", home + "/.aws"})

	for path, want := range map[string]landlockAccess{
		home:                   listDir,
		home + "/.config":      listDir,
		home + "/src":          readTree,
		home + "/.config/nvim": readTree,
		home + "/.bashrc":      readFile,
		filepath.Dir(home):     listDir,
		"/":                    listDir,
	} {
		got, ok := grants[path]
		if !ok || got != want {
			t.Errorf("%s: granted %d (present %v), want %d", path, got, ok, want)
		}
	}

	for _, path := range []string{home + "/.ssh", home + "/.config/gh", home + "/.aws", home + "/loop"} {
		if access, ok := grants[path]; ok {
			t.Errorf("%s is granted %d; a hidden path, or a link that could reach one, must have no rule", path, access)
		}
	}
}

// fakeHome has two hidden directories, a sibling of each, a dotfile, and a link back to itself.
func fakeHome(t *testing.T) string {
	t.Helper()

	home := mustResolve(t, t.TempDir())

	for _, dir := range []string{".ssh", ".config/gh", ".config/nvim", "src"} {
		err := os.MkdirAll(filepath.Join(home, dir), 0o700)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.WriteFile(filepath.Join(home, ".bashrc"), nil, 0o600)
	if err == nil {
		err = os.Symlink(home, filepath.Join(home, "loop"))
	}

	if err != nil {
		t.Fatal(err)
	}

	return home
}

// A dotfile manager's symlinked home: the kernel checks the real path, so the ancestors that must stay list-only are the real ones.
func TestLandlockGrantsFollowASymlinkedHomeToItsRealPath(t *testing.T) {
	t.Parallel()

	target := mustResolve(t, t.TempDir())
	link := filepath.Join(mustResolve(t, t.TempDir()), "home")

	err := os.Mkdir(filepath.Join(target, ".ssh"), 0o700)
	if err == nil {
		err = os.Symlink(target, link)
	}

	if err != nil {
		t.Fatal(err)
	}

	grants := grantsByPath(t, nil, []string{link + "/.ssh"})

	if access, ok := grants[target]; !ok || access != listDir {
		t.Errorf("the real home %s is granted %d (present %v); want list only", target, access, ok)
	}

	if _, ok := grants[target+"/.ssh"]; ok {
		t.Errorf("the real .ssh is granted")
	}
}

func TestLandlockGrantsRefuseAWritableDirAboveAHiddenPath(t *testing.T) {
	t.Parallel()

	home := mustResolve(t, t.TempDir())

	_, err := landlockGrants([]string{home}, []string{home + "/.ssh"})
	if err == nil || !strings.Contains(err.Error(), "cannot hide") {
		t.Errorf("landlockGrants = %v; want a refusal, since a write grant on %s covers its .ssh", err, home)
	}

	grants := grantsByPath(t, []string{home + "/work"}, []string{home + "/.ssh"})
	if grants[home+"/work"] != writeTree || grants["/dev/null"] != writeFile {
		t.Errorf("the workspace and /dev/null are not writable: %v", grants)
	}
}

// The default layout puts the build trees in workspace/ beside the database, so hiding the database must leave a step there writable and its siblings readable, never refuse it.
func TestLandlockGrantsHideTheStateDatabaseBesideItsWorkspace(t *testing.T) {
	t.Parallel()

	root := mustResolve(t, t.TempDir())
	state := filepath.Join(root, ".steps")
	build := filepath.Join(state, "workspace", "build")

	err := os.MkdirAll(build, 0o700)
	for _, file := range []string{"steps.db", "steps.db-wal", "notes"} {
		if err == nil {
			err = os.WriteFile(filepath.Join(state, file), nil, 0o600)
		}
	}

	if err != nil {
		t.Fatal(err)
	}

	hidden := []string{state + "/steps.db", state + "/steps.db-wal", state + "/steps.db-shm"}
	grants := grantsByPath(t, []string{build}, hidden)

	for path, want := range map[string]landlockAccess{
		root:                 listDir,
		state:                listDir,
		state + "/notes":     readFile,
		state + "/workspace": readTree,
		build:                writeTree,
	} {
		got, ok := grants[path]
		if !ok || got != want {
			t.Errorf("%s: granted %d (present %v), want %d", path, got, ok, want)
		}
	}

	for _, path := range hidden {
		if access, ok := grants[path]; ok {
			t.Errorf("%s is granted %d", path, access)
		}
	}
}
