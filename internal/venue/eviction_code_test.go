package venue

// What a signalled exit LOOKS like depends on which runner ran the command,
// and a placed step has two.

import (
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/wire"
)

// TestAContainerKilledByAReclamationIsInfrastructure is the seam between the
// two placement paths.
//
// Executed directly, the shim reports os/exec's -1 for a signalled command.
// Run in a container ON the worker, the code comes from `docker exec`, which
// reports a signal-killed process as 128+N and can never say -1 — so the
// classification could not fire on the container path at all, and AWS taking
// the machine was billed to the pipeline author's attempts: budget as the
// step's own verdict.
func TestAContainerKilledByAReclamationIsInfrastructure(t *testing.T) {
	for _, code := range []int{shell.SignalledExitCode, 137, 143} {
		session := &session{}
		session.drain.Store(&wire.Draining{Reason: "spot interruption", Terminal: true})

		err := runner{session: session}.asEviction(&shell.ExitError{Command: "make", Code: code})
		if !errors.Is(err, ErrEvicted) {
			t.Errorf("exit %d on a reclaimed worker = %v, want ErrEvicted", code, err)
		}
	}
}

// TestAContainersOwnVerdictOnAReclaimedWorkerStands is the other half of the
// same line: widening what counts as signalled must not swallow a command
// that ran and chose a status.
func TestAContainersOwnVerdictOnAReclaimedWorkerStands(t *testing.T) {
	for _, code := range []int{1, 2, 3, 127} {
		session := &session{}
		session.drain.Store(&wire.Draining{Reason: "spot interruption", Terminal: true})

		err := runner{session: session}.asEviction(&shell.ExitError{Command: "make", Code: code})
		if errors.Is(err, ErrEvicted) {
			t.Errorf("exit %d was re-read as an eviction: %v", code, err)
		}
	}
}
