package venue

// A placed container outliving the step that was cancelled mid-command.

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// ownContainers lists every container this test process owns on the local daemon, running or not.
func ownContainers(t *testing.T) []string {
	t.Helper()

	//nolint:gosec // a fixed binary; the only computed argument is this process's own pid
	out, err := exec.CommandContext(t.Context(), "docker", "ps", "-aq", "--no-trunc",
		"--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}

	return strings.Fields(string(out))
}

// TestACancelledCommandStillRemovesItsContainer is a placed step's timeout landing mid-command: Close must still remove the worker's container, where a handoff outrunning its grace marked the session broken and teardown then declined the removal (measured: a timed-out self-build implementer's container still running eight hours later); compared before/after so another test's container is not read as this one's leak.
func TestACancelledCommandStillRemovesItsContainer(t *testing.T) {
	requireDockerVenue(t)

	before := ownContainers(t)

	previousGrace := dockerHandoffGrace
	dockerHandoffGrace = time.Nanosecond

	t.Cleanup(func() { dockerHandoffGrace = previousGrace })

	spec := localWorker(t, t.TempDir())
	spec.Image = "alpine:3"

	placed, err := NewRunner(spec)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, _, _ = placed.RunCaptureFullLimitedStreamed(ctx, "while :; do echo tick; sleep 0.01; done", 1<<20, "")

	_ = placed.Close()

	var leaked []string

	for _, id := range ownContainers(t) {
		if !slices.Contains(before, id) {
			leaked = append(leaked, id)
		}
	}

	if len(leaked) > 0 {
		t.Cleanup(func() {
			for _, id := range leaked {
				_ = shell.RemoveContainer(context.Background(), "", id)
			}
		})
		t.Fatalf("containers %v outlived Close after a cancelled command; want the step's container removed", leaked)
	}
}
