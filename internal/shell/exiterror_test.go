package shell

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// hostFailure runs a command that fails on this machine, so a venue-reported
// failure can be compared against what os/exec actually produces rather than
// against an assumption about it.
func hostFailure(t *testing.T, command string) error {
	t.Helper()

	err := HostRunner{cwd: t.TempDir()}.Run(context.Background(), command)
	if err == nil {
		t.Fatalf("HostRunner.Run(%q) succeeded, want a failure to compare against", command)
	}

	return err
}

// TestExitErrorAnswersAlikeFromEitherSource is the parity that makes remote
// execution safe to classify: a command that ran and exited nonzero must
// answer the same three questions the same way whether os/exec saw it or a
// venue reported it over a wire. Everything downstream reads only these three
// — classifyRunError deciding Fail vs errored, guard.go deciding "said no" vs
// "could not run" — so a divergence here is a divergence in what a red build
// means.
func TestExitErrorAnswersAlikeFromEitherSource(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"host", hostFailure(t, "exit 3")},
		{"venue", &ExitError{Command: "exit 3", Venue: "gpu", Code: 3}},
	} {
		if !IsExitError(tc.err) {
			t.Errorf("%s: IsExitError = false, want true — a nonzero exit is the step failing, not the machinery", tc.name)
		}

		if !processStarted(tc.err) {
			t.Errorf("%s: processStarted = false, want true", tc.name)
		}

		if got := exitCodeOf(tc.err); got != 3 {
			t.Errorf("%s: exitCodeOf = %d, want 3", tc.name, got)
		}
	}
}

// TestExitErrorSurvivesWrapping pins that the classification reaches through
// the wraps every caller adds — task.go names the task, guard.go names the
// guard.
func TestExitErrorSurvivesWrapping(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("task %q: %w", "build", &ExitError{Command: "exit 7", Venue: "gpu", Code: 7})

	if !IsExitError(wrapped) {
		t.Error("IsExitError through a wrap = false, want true")
	}

	if got := exitCodeOf(wrapped); got != 7 {
		t.Errorf("exitCodeOf through a wrap = %d, want 7", got)
	}
}

// TestNeverStartedIsNotAnExitError is the distinction guard.go depends on. A
// venue reports an unreachable worker as a plain error, never an ExitError, so
// that "the machine is down" can never be read as "the guard said false".
func TestNeverStartedIsNotAnExitError(t *testing.T) {
	t.Parallel()

	neverStarted := errors.New(`worker "gpu": command "true" never started: dial tcp: connection refused`)

	if IsExitError(neverStarted) {
		t.Error("IsExitError = true for a command that never ran, want false — this is infrastructure, not a step failure")
	}

	if processStarted(neverStarted) {
		t.Error("processStarted = true for a command that never ran, want false")
	}

	if got := exitCodeOf(neverStarted); got != -1 {
		t.Errorf("exitCodeOf = %d, want -1", got)
	}
}

// TestSignalledCommandReportsMinusOne covers the ambiguous code. A local kill
// is an *exec.ExitError whose ExitCode() is -1; a venue reporting a signalled
// command sends the same -1. Both mean "started, then died", which is why
// processStarted rather than the code is the question callers ask.
func TestSignalledCommandReportsMinusOne(t *testing.T) {
	t.Parallel()

	remoteKill := &ExitError{Command: "sleep 60", Venue: "gpu", Code: -1}

	if !processStarted(remoteKill) {
		t.Error("processStarted = false for a signalled remote command, want true")
	}

	if got := exitCodeOf(remoteKill); got != -1 {
		t.Errorf("exitCodeOf = %d, want -1", got)
	}
}

// TestExitErrorNamesTheVenue: a red build on a fleet that does not say which
// machine sends the operator to look at the wrong box.
func TestExitErrorNamesTheVenue(t *testing.T) {
	t.Parallel()

	err := &ExitError{Command: "make test", Venue: "gpu", Code: 2}

	for _, want := range []string{"make test", "gpu", "2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to mention %q", err.Error(), want)
		}
	}
}
