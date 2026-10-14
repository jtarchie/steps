package venue

// What every placed runner shares: when a failure is the machine leaving rather than the command answering.

import (
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// ErrEvicted is a step whose worker was taken away underneath it: a deliberate divergence from Concourse, which errors the build, because spending an author's attempts: on the cloud reclaiming a machine charges them for what they neither caused nor can fix.
var ErrEvicted = errors.New("the worker was reclaimed while the step was running")

// closeTimeout bounds a teardown, which runs from deferred paths whose context has usually already ended: leaving scratch on somebody else's machine would be worst exactly then.
var closeTimeout = 30 * time.Second //nolint:gochecknoglobals // a test seam for a wait on another machine

// ReclaimedBy reports whether the worker a runner used said it was definitely
// going away, and what it said.
//
// Asked of a finished runner, because the two-minute window means a step
// often succeeds on a machine that is being reclaimed — and nothing about
// that success says so. A caller holding the machine for later steps needs to
// know to let it go.
//
// A runner that is not placed answers false, so a caller need not ask whether
// it is talking to a venue.
func ReclaimedBy(runner shell.Runner) (string, bool) {
	placed, ok := runner.(interface{ reclaimed() (string, bool) })
	if !ok {
		return "", false
	}

	return placed.reclaimed()
}

// asEvictionOf reads a failure on a machine that announced its own reclamation as an eviction, unless the command answered for itself: a signalled exit is the machine ending it, any other exit code is the step's verdict.
func asEvictionOf(err error, reason string, reclaimed bool) error {
	if err == nil || !reclaimed {
		return err
	}

	if shell.IsExitError(err) && !signalledExit(exitCodeOf(err)) {
		return err
	}

	if reason == "" {
		reason = "no reason given"
	}

	return fmt.Errorf("%w (%s): %w", ErrEvicted, reason, err)
}

// signalledExit reports the codes that mean "the machine ended this command"
// rather than "the command answered".
//
// Two spellings, because a placed step has two runners. Over ssh, a command
// killed by a signal reports os/exec's own -1. Run in a CONTAINER, the code
// comes from `docker exec`, which reports a signal-killed process as 128+N
// and can never say -1 — reading only one spelling billed a reclamation to
// the pipeline author's attempts: budget as the step's own verdict.
//
// Only consulted once the worker has ALREADY said it is being reclaimed, so a
// container that legitimately exits 137 on a healthy machine — an OOM kill of
// its own making — is still the command answering.
func signalledExit(code int) bool {
	const (
		sigkillExit = 137
		sigtermExit = 143
		sigintExit  = 130
		sighupExit  = 129
		sigquitExit = 131
	)

	switch code {
	case shell.SignalledExitCode, sighupExit, sigintExit, sigquitExit, sigkillExit, sigtermExit:
		return true
	default:
		return false
	}
}

// exitCodeOf reads the status back off an error this package produced.
func exitCodeOf(err error) int {
	var exitErr *shell.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.Code
	}

	return -1
}
