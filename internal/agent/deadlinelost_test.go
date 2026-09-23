package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// stallingRunner fails only once its context ends, the way a placed command's docker handoff does when the step's deadline cancels it mid-flight.
type stallingRunner struct{ shell.Runner }

func (stallingRunner) RunCaptureFullLimitedStreamed(ctx context.Context, _ string, _ int, _ string) (string, string, int, error) {
	<-ctx.Done()

	return "", "", 0, errors.New("the worker did not finish with the docker socket after 2s")
}

func (r stallingRunner) RunCaptureFullLimited(ctx context.Context, command string, maxBytes int, spillDir string) (string, string, int, error) {
	return r.RunCaptureFullLimitedStreamed(ctx, command, maxBytes, spillDir)
}

// TestADeadlineEndingACommandIsNotALostTree: a command the step's own timeout: cut off says nothing about the container, and filing it as lost made the step's error "the step's container could not be reached" in place of the deadline (measured: a placed self-build implementer at exactly its 2h ceiling).
func TestADeadlineEndingACommandIsNotALostTree(t *testing.T) {
	t.Parallel()

	runner := stallingRunner{}
	lost := &lostTree{}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	env := toolEnv{dir: t.TempDir(), runner: runner, tree: containerTree{runner: runner, lost: lost}, lost: lost}
	shellToolResult(ctx, "sleep 60", env, maxToolOutputBytes)

	_, _, _, err := containerTree{runner: runner, lost: lost}.execute(ctx, "true")
	if err == nil {
		t.Error("execute returned no error; want the runner's failure passed back to the tool")
	}

	taken := lost.taken()
	if taken != nil {
		t.Errorf("lost = %v; want nothing recorded when the step's own deadline ended the command", taken)
	}
}
