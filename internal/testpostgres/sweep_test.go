package testpostgres

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// signalled records the pids terminate is asked for, instead of signalling them.
func signalled(t *testing.T) *[]int {
	t.Helper()

	var pids []int

	orig := terminate
	terminate = func(pid int) { pids = append(pids, pid) }

	t.Cleanup(func() { terminate = orig })

	return &pids
}

// child runs sleep through a symlink of the given name, so ps reports that name for its pid; a platform that cannot run it that way skips.
func child(t *testing.T, name string) int {
	t.Helper()

	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep to run: %v", err)
	}

	path := filepath.Join(t.TempDir(), name)

	err = os.Symlink(sleep, path)
	if err != nil {
		t.Skipf("cannot symlink sleep: %v", err)
	}

	cmd := exec.Command(path, "60") //nolint:gosec,noctx // a symlink to sleep this test just made
	err = cmd.Start()
	if err != nil {
		t.Skipf("cannot run sleep as %q: %v", name, err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	reported, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(cmd.Process.Pid)).Output() //nolint:gosec,noctx // a probe of the pid this test just started
	if err != nil || !strings.HasSuffix(strings.TrimSpace(string(reported)), name) {
		t.Skipf("ps does not report %q for it (%q, %v)", name, reported, err)
	}

	return cmd.Process.Pid
}

func pidFile(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "postmaster.pid")

	err := os.WriteFile(path, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// A dead run's postmaster is stopped, but only a process that still is a postgres: its pid may since belong to anything.
func TestStopPostmasterSignalsOnlyAProcessNamedPostgres(t *testing.T) {
	signals := signalled(t)

	postgres := child(t, "postgres")
	other := child(t, "sleep")

	stopPostmaster(pidFile(t, strconv.Itoa(other)+"\n/data\n"))

	if len(*signals) != 0 {
		t.Fatalf("a pid file naming a process that is not a postgres signalled %v", *signals)
	}

	stopPostmaster(pidFile(t, strconv.Itoa(postgres)+"\n/data\n"))

	if len(*signals) != 1 || (*signals)[0] != postgres {
		t.Errorf("signalled %v, want just the postgres %d", *signals, postgres)
	}
}

// A pid file that does not name a real server, or names a pid that must never be signalled, is left alone.
func TestStopPostmasterIgnoresAnUnusablePidFile(t *testing.T) {
	signals := signalled(t)

	for name, contents := range map[string]string{
		"empty":    "",
		"garbage":  "not a pid\n",
		"zero":     "0\n",
		"one":      "1\n",
		"negative": "-7\n",
		"none":     "99999999\n",
	} {
		stopPostmaster(pidFile(t, contents))

		if len(*signals) != 0 {
			t.Errorf("%s: signalled %v", name, *signals)
		}
	}

	stopPostmaster(filepath.Join(t.TempDir(), "absent.pid"))

	if len(*signals) != 0 {
		t.Errorf("a missing pid file signalled %v", *signals)
	}
}

func TestAliveTellsALiveProcessFromAGoneOne(t *testing.T) {
	t.Parallel()

	if !alive(os.Getpid()) {
		t.Error("this process is alive")
	}

	if alive(math.MaxInt32 - 1) {
		t.Error("a pid nothing holds is not alive")
	}

	// A process another user owns answers EPERM, which is still a live process.
	if errors.Is(syscall.Kill(1, 0), syscall.EPERM) && !alive(1) {
		t.Error("a live process owned by another user reads as gone")
	}
}

// sweep removes the work directory of a test process that is gone and stops its postmaster, and leaves a live process's directory and anything it did not make.
func TestSweepRemovesOnlyTheDirectoriesOfDeadProcesses(t *testing.T) {
	signals := signalled(t)

	t.Setenv("TMPDIR", t.TempDir())

	root := workRoot()
	postgres := child(t, "postgres")

	dead := filepath.Join(root, strconv.Itoa(math.MaxInt32-1))
	live := filepath.Join(root, strconv.Itoa(os.Getpid()))
	stranger := filepath.Join(root, "not-a-pid")

	for _, dir := range []string{dead, live, stranger} {
		err := os.MkdirAll(filepath.Join(dir, "data"), 0o750)
		if err != nil {
			t.Fatal(err)
		}
	}

	err := os.WriteFile(filepath.Join(dead, "data", "postmaster.pid"), []byte(strconv.Itoa(postgres)+"\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	sweep()

	_, err = os.Stat(dead)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a dead process's directory = %v, want it removed", err)
	}

	for _, dir := range []string{live, stranger} {
		_, err = os.Stat(dir)
		if err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(dir), err)
		}
	}

	if len(*signals) != 1 || (*signals)[0] != postgres {
		t.Errorf("signalled %v, want just the dead run's postmaster %d", *signals, postgres)
	}
}

func TestStopOnANilServerDoesNothing(t *testing.T) {
	t.Parallel()

	var server *Server

	server.Stop()
}
