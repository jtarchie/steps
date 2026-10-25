//go:build unix

package shell

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
)

func headless() context.Context {
	return events.WithOutput(context.Background(), events.Output{Headless: true})
}

func pgidOf(ctx context.Context, t *testing.T) int {
	t.Helper()

	out, err := RunCapture(ctx, HostRunner{cwd: t.TempDir()}, "ps -o pgid= -p $$")
	if err != nil {
		t.Fatal(err)
	}

	pgid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("ps printed %q: %v", out, err)
	}

	return pgid
}

// Headless shares no group with the daemon (a tty read would stop both); interactive stays foreground so its prompts work.
func TestHostCommandGroup(t *testing.T) {
	t.Parallel()

	t.Run("headless: a group of its own", func(t *testing.T) {
		t.Parallel()

		if pgid := pgidOf(headless(), t); pgid == syscall.Getpgrp() {
			t.Errorf("headless command ran in this process's group %d", pgid)
		}
	})

	t.Run("interactive: this process's group", func(t *testing.T) {
		t.Parallel()

		if pgid, want := pgidOf(context.Background(), t), syscall.Getpgrp(); pgid != want {
			t.Errorf("interactive command ran in group %d, want %d", pgid, want)
		}
	})
}

func exited(pid int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}

		time.Sleep(20 * time.Millisecond)
	}

	return false
}

func readPid(t *testing.T, path string) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path) //nolint:gosec // a path this test made
		if err == nil {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr == nil {
				return pid
			}
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("no pid in %s", path)

	return 0
}

// A cancelled step takes what it started with it, not just its shell.
func TestHeadlessCancelKillsGrandchildren(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	ctx, cancel := context.WithTimeout(headless(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()

	err := Run(ctx, HostRunner{cwd: dir}, "sleep 60 & echo $! > pid; wait")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want it to satisfy errors.Is(_, context.DeadlineExceeded)", err)
	}

	if time.Since(start) > cancelWaitDelay {
		t.Errorf("Run took %s: it waited out the grandchild instead of killing it", time.Since(start))
	}

	if pid := readPid(t, filepath.Join(dir, "pid")); !exited(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d outlived its cancelled step", pid)
	}
}

// KillAll is the second ^C: every headless group dies now, not when its step is cancelled.
//
//nolint:paralleltest // KillAll reaches every live group in the process
func TestKillAll(t *testing.T) {
	dir := t.TempDir()
	done := make(chan error, 1)

	go func() {
		done <- Run(headless(), HostRunner{cwd: dir}, "sleep 60 & echo $! > pid; wait")
	}()

	pid := readPid(t, filepath.Join(dir, "pid"))

	KillAll()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a killed command reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after KillAll")
	}

	if !exited(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("grandchild %d outlived KillAll", pid)
	}
}

// A child in its own group under this process is never orphaned, so without the inherited ignore its SIGTTIN would stop it for real.
//
//nolint:paralleltest // signal dispositions are process-wide
func TestIgnoreTerminalStopsIsInherited(t *testing.T) {
	IgnoreTerminalStops()
	t.Cleanup(func() { signal.Reset(syscall.SIGTTIN, syscall.SIGTTOU) })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", "kill -TTIN $$; kill -TTOU $$; echo survived")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}

	if string(out) != "survived\n" {
		t.Errorf("stdout = %q, want survived", out)
	}
}
