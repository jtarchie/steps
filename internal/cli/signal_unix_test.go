//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
)

func TestWatchSignals(t *testing.T) {
	t.Parallel()

	sigs := make(chan os.Signal, 1)
	stopped := make(chan struct{})
	cancelled := make(chan struct{})
	nowGot := make(chan os.Signal, 1)
	done := make(chan struct{})

	go func() {
		watchSignals(context.Background(), sigs, stopped, func() { close(cancelled) }, func(sig os.Signal) { nowGot <- sig })
		close(done)
	}()

	sigs <- syscall.SIGTERM

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the first signal did not cancel")
	}

	select {
	case sig := <-nowGot:
		t.Fatalf("the first signal went to now (%v); it only cancels", sig)
	case <-time.After(50 * time.Millisecond):
	}

	sigs <- syscall.SIGINT

	select {
	case sig := <-nowGot:
		if sig != syscall.SIGINT {
			t.Errorf("now got %v, want the second signal, SIGINT", sig)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal was swallowed")
	}

	<-done
}

const signalHelperEnv = "STEPS_TEST_SIGNAL_HELPER"

// The daemon's shape at its worst: a child kept alive past the first ^C (a hook's grace) and a shutdown that never finishes.
func signalHelper(dir string) {
	ctx, cancel := withSignalCancel(events.WithOutput(context.Background(), events.Output{Headless: true}))
	defer cancel()

	runner, _ := shell.NewRunner(shell.RunnerSpec{Cwd: dir})

	go func() {
		_ = shell.Run(context.WithoutCancel(ctx), runner, "sleep 60 & echo $! > pid.tmp && mv pid.tmp pid; wait")
	}()

	select {}
}

func TestSecondSignalKillsChildrenAndExits(t *testing.T) { //nolint:paralleltest // the helper half must not be one
	if dir := os.Getenv(signalHelperEnv); dir != "" {
		signalHelper(dir)
	}

	dir := t.TempDir()
	helper, exited := startSignalHelper(t, dir)
	grandchild := waitForPid(t, filepath.Join(dir, "pid"))

	t.Cleanup(func() { _ = syscall.Kill(grandchild, syscall.SIGKILL) })

	_ = helper.Process.Signal(syscall.SIGINT)

	select {
	case err := <-exited:
		t.Fatalf("the first signal ended the process (%v); it should only start the shutdown", err)
	case <-time.After(300 * time.Millisecond):
	}

	_ = helper.Process.Signal(syscall.SIGINT)

	select {
	case err := <-exited:
		assertDiedOf(t, err, syscall.SIGINT)
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not end a process whose shutdown was stuck")
	}

	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(grandchild, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d outlived the second signal", grandchild)
		}
	}
}

func startSignalHelper(t *testing.T, dir string) (*exec.Cmd, <-chan error) {
	t.Helper()

	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSecondSignalKillsChildrenAndExits$") //nolint:gosec // this test binary
	helper.Env = append(os.Environ(), signalHelperEnv+"="+dir)

	err := helper.Start()
	if err != nil {
		t.Fatal(err)
	}

	exited := make(chan error, 1)

	go func() { exited <- helper.Wait() }()

	return helper, exited
}

func waitForPid(t *testing.T, path string) int {
	t.Helper()

	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		raw, err := os.ReadFile(path) //nolint:gosec // a path this test made
		if err != nil {
			continue
		}

		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err == nil {
			return pid
		}
	}

	t.Fatalf("no pid in %s", path)

	return 0
}

// A supervisor reads its own signal back only from a death by that signal, never from an exit code.
func assertDiedOf(t *testing.T, err error, sig syscall.Signal) {
	t.Helper()

	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("helper exited with %v", err)
	}

	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != sig {
		t.Errorf("helper exit = %v, want death by %v", err, sig)
	}
}
