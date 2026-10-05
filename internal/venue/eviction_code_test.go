package venue

// What a signalled exit LOOKS like depends on which runner ran the command,
// and a placed step has two.

import (
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestAContainerKilledByAReclamationIsInfrastructure is the seam between the
// two placement paths.
//
// Run bare, a signalled command reports os/exec's -1; run in a container the
// code comes from `docker exec`, which reports 128+N and can never say -1 —
// so both have to read as the machine ending the command, or AWS taking it
// is billed to the pipeline author's attempts: budget.
func TestAContainerKilledByAReclamationIsInfrastructure(t *testing.T) {
	for _, code := range []int{shell.SignalledExitCode, 137, 143} {
		err := asEvictionOf(&shell.ExitError{Command: "make", Code: code}, "spot interruption", true)
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
		err := asEvictionOf(&shell.ExitError{Command: "make", Code: code}, "spot interruption", true)
		if errors.Is(err, ErrEvicted) {
			t.Errorf("exit %d was re-read as an eviction: %v", code, err)
		}
	}
}
