package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunShellCaptureFull(t *testing.T) {
	t.Parallel()

	t.Run("captures stdout, stderr, and exit code separately", func(t *testing.T) {
		t.Parallel()

		stdout, stderr, exitCode, err := RunShellCaptureFull(context.Background(), "echo out; echo err >&2; exit 3", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull: %v", err)
		}

		if stdout != "out\n" {
			t.Errorf("stdout = %q, want %q", stdout, "out\n")
		}

		if stderr != "err\n" {
			t.Errorf("stderr = %q, want %q", stderr, "err\n")
		}

		if exitCode != 3 {
			t.Errorf("exitCode = %d, want 3", exitCode)
		}
	})

	t.Run("successful command has exit code 0 and a nil error", func(t *testing.T) {
		t.Parallel()

		_, _, exitCode, err := RunShellCaptureFull(context.Background(), "true", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull: %v", err)
		}

		if exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", exitCode)
		}
	})

	t.Run("a nonzero exit is not a Go error", func(t *testing.T) {
		t.Parallel()

		_, _, exitCode, err := RunShellCaptureFull(context.Background(), "exit 1", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCaptureFull returned a Go error for a normal nonzero exit: %v", err)
		}

		if exitCode != 1 {
			t.Errorf("exitCode = %d, want 1", exitCode)
		}
	})

	t.Run("a failure to start the process returns a Go error", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := RunShellCaptureFull(context.Background(), "true", "/nonexistent/dir/for/sure")
		if err == nil {
			t.Error("expected an error when cwd doesn't exist")
		}
	})
}

// TestRunShellCaptureFullSignalKilled guards against exitCodeOf's -1
// sentinel being ambiguous between "never started" and "ran, then
// signal-killed" — both produce ExitCode() == -1, so a naive check
// misclassifies a context-timeout kill as a start failure and drops its
// captured output.
func TestRunShellCaptureFullSignalKilled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancelled once the command SAYS it is running, rather than after a fixed
	// wall-clock delay. A short deadline races the process launch, and loses
	// it on a loaded machine — the kill then lands before anything started,
	// which is a start failure, which is the very case this test exists to
	// tell apart from a signalled one. It failed exactly that way once the
	// suite around it got heavier.
	watching := make(chan struct{})

	go func() {
		defer close(watching)

		for range 2000 {
			_, statErr := os.Stat(ready)
			if statErr == nil {
				cancel()

				return
			}

			time.Sleep(time.Millisecond)
		}
	}()

	t.Cleanup(func() { cancel(); <-watching })

	// exec replaces the shell with sleep (same PID), so the context's
	// SIGKILL lands directly on it instead of orphaning a grandchild that
	// would keep the output pipe open until it exits on its own — keeps
	// this test fast instead of blocking for the full sleep duration.
	stdout, _, exitCode, err := RunShellCaptureFull(ctx, "echo partial; touch "+ready+"; exec sleep 30", dir)
	if err != nil {
		t.Fatalf("a signal-killed process must be reported as data, not a Go error (regression: it was misclassified as \"failed to start\"): %v", err)
	}

	if stdout != "partial\n" {
		t.Errorf("stdout = %q, want the output captured before the kill", stdout)
	}

	if exitCode != -1 {
		t.Errorf("exitCode = %d, want -1 for a signal-killed process", exitCode)
	}
}

func TestRunShellCapture(t *testing.T) {
	t.Parallel()

	t.Run("returns captured stdout on success", func(t *testing.T) {
		t.Parallel()

		out, err := RunShellCapture(context.Background(), "echo hello", t.TempDir())
		if err != nil {
			t.Fatalf("RunShellCapture: %v", err)
		}

		if string(out) != "hello\n" {
			t.Errorf("out = %q, want %q", out, "hello\n")
		}
	})

	t.Run("a nonzero exit is a Go error, unlike RunShellCaptureFull", func(t *testing.T) {
		t.Parallel()

		out, err := RunShellCapture(context.Background(), "echo partial; exit 1", t.TempDir())
		if err == nil {
			t.Fatal("expected an error for a nonzero exit")
		}

		if out != nil {
			t.Errorf("out = %q, want nil on error", out)
		}
	})
}

// TestHostRunnerScrubsNonAllowlistedEnv guards the fix for the security
// finding that every host-executed command inherited the full steps process
// environment, including any secret an operator happened to have exported
// (e.g. a configured agent's api_key_env). A host-executed command must see
// only the fixed allowlist (hostEnvAllowlist) — PATH included, so ordinary
// tooling still resolves — and nothing else, even a variable set on this
// very test process immediately before the command runs. Not run in
// parallel: t.Setenv forbids it.
func TestHostRunnerScrubsNonAllowlistedEnv(t *testing.T) {
	t.Setenv("STEPS_TEST_SECRET", "leaked-if-visible")

	stdout, _, exitCode, err := RunShellCaptureFull(context.Background(), `echo "[$STEPS_TEST_SECRET]"`, t.TempDir())
	if err != nil {
		t.Fatalf("RunShellCaptureFull: %v", err)
	}

	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}

	if stdout != "[]\n" {
		t.Errorf("stdout = %q, want %q (STEPS_TEST_SECRET must not reach a host-executed command)", stdout, "[]\n")
	}

	pathOut, _, _, err := RunShellCaptureFull(context.Background(), `echo "[$PATH]"`, t.TempDir())
	if err != nil {
		t.Fatalf("RunShellCaptureFull: %v", err)
	}

	if pathOut == "[]\n" {
		t.Error("PATH was scrubbed too; allowlisted variables must still reach the command")
	}
}

func TestWrapIfCanceled(t *testing.T) {
	t.Parallel()

	t.Run("ctx not canceled returns err unchanged", func(t *testing.T) {
		t.Parallel()

		original := errors.New("boom")

		got := wrapIfCanceled(context.Background(), original)
		if !errors.Is(got, original) {
			t.Errorf("wrapIfCanceled = %v, want it to still be (or wrap) %v", got, original)
		}

		if errors.Is(got, context.Canceled) {
			t.Error("wrapIfCanceled claimed cancellation for a live context")
		}
	})

	t.Run("ctx canceled wraps both the original error and ctx.Err()", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		original := errors.New("boom")

		got := wrapIfCanceled(ctx, original)
		if !errors.Is(got, context.Canceled) {
			t.Errorf("wrapIfCanceled = %v, want it to satisfy errors.Is(_, context.Canceled)", got)
		}

		if !errors.Is(got, original) {
			t.Errorf("wrapIfCanceled = %v, want it to still wrap the original error %v", got, original)
		}
	})
}

func TestCanceledError(t *testing.T) {
	t.Parallel()

	t.Run("live context returns nil", func(t *testing.T) {
		t.Parallel()

		err := CanceledError(context.Background())
		if err != nil {
			t.Errorf("CanceledError = %v, want nil", err)
		}
	})

	t.Run("canceled context returns an error satisfying errors.Is", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := CanceledError(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("CanceledError = %v, want it to satisfy errors.Is(_, context.Canceled)", err)
		}
	})

	t.Run("deadline-exceeded context is distinguishable via errors.Is", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()

		time.Sleep(time.Millisecond)

		err := CanceledError(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("CanceledError = %v, want it to satisfy errors.Is(_, context.DeadlineExceeded)", err)
		}
	})
}

// TestHostRunnerCancellationIsDetectable confirms Run/RunCapture (which
// already error on any nonzero exit) make that error's chain satisfy
// errors.Is against context.Canceled/DeadlineExceeded when the command was
// killed because ctx was canceled — the mechanism internal/pipeline and
// internal/trigger rely on to tell a shutdown-interrupted step apart from a
// step that genuinely failed on its own.
func TestHostRunnerCancellationIsDetectable(t *testing.T) {
	t.Parallel()

	t.Run("Run", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		runner := HostRunner{cwd: t.TempDir()}

		err := runner.Run(ctx, "exec sleep 5")
		if err == nil {
			t.Fatal("expected an error for a command killed by a canceled context")
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run error = %v, want it to satisfy errors.Is(_, context.DeadlineExceeded)", err)
		}
	})

	t.Run("RunCapture", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		runner := HostRunner{cwd: t.TempDir()}

		_, err := runner.RunCapture(ctx, "exec sleep 5")
		if err == nil {
			t.Fatal("expected an error for a command killed by a canceled context")
		}

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RunCapture error = %v, want it to satisfy errors.Is(_, context.DeadlineExceeded)", err)
		}
	})

	t.Run("a genuine failure with a live context does not falsely claim cancellation", func(t *testing.T) {
		t.Parallel()

		runner := HostRunner{cwd: t.TempDir()}

		err := runner.Run(context.Background(), "exit 1")
		if err == nil {
			t.Fatal("expected an error for a nonzero exit")
		}

		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Run error = %v, want it to NOT satisfy errors.Is against context.Canceled/DeadlineExceeded for an ordinary failure", err)
		}
	})
}

// TestHostEnvWithOptsInNamedVariables covers the env: escape hatch on the host
// path: hostEnvAllowlist stays the default trust boundary, and a pipeline can
// name one more variable through it without widening that default for
// everything else.
func TestHostEnvWithOptsInNamedVariables(t *testing.T) {
	t.Setenv("STEPS_TEST_OPTED_IN", "yes")
	t.Setenv("STEPS_TEST_NOT_OPTED_IN", "no")

	env := hostEnvWith([]string{"STEPS_TEST_OPTED_IN"})

	if !slices.Contains(env, "STEPS_TEST_OPTED_IN=yes") {
		t.Errorf("env = %v, want the opted-in variable to be present", env)
	}

	for _, kv := range env {
		if strings.HasPrefix(kv, "STEPS_TEST_NOT_OPTED_IN=") {
			t.Errorf("env = %v, want a variable that was not named to stay out", env)
		}
	}
}

// TestHostEnvWithSkipsUnsetNames pins that naming a variable nobody exported
// contributes nothing rather than an empty value: a command testing for
// presence must be able to tell "unset" from "set to empty", and inventing the
// latter would turn a forgotten export into a silent misconfiguration.
func TestHostEnvWithSkipsUnsetNames(t *testing.T) {
	env := hostEnvWith([]string{"STEPS_TEST_DEFINITELY_UNSET"})

	for _, kv := range env {
		if strings.HasPrefix(kv, "STEPS_TEST_DEFINITELY_UNSET") {
			t.Errorf("env = %v, want an unset name to contribute nothing", env)
		}
	}
}

// TestHostEnvUnchangedWithoutOptIn keeps the no-env: case byte-identical to
// what HostEnv always returned.
func TestHostEnvUnchangedWithoutOptIn(t *testing.T) {
	if got, want := strings.Join(hostEnvWith(nil), "\n"), strings.Join(HostEnv(), "\n"); got != want {
		t.Errorf("hostEnvWith(nil) = %q, want it identical to HostEnv() = %q", got, want)
	}
}
