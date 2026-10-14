package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/agent/tree"

	"github.com/jtarchie/steps/internal/shell"
)

type tooloutputBrokenRunner struct{ shell.Runner }

func (tooloutputBrokenRunner) Exec(context.Context, shell.Command) (shell.Result, error) {
	return shell.Result{}, errors.New("no such container")
}

func TestARunnerFailureOnALiveStepIsALostTree(t *testing.T) {
	t.Parallel()

	runner := tooloutputBrokenRunner{}
	lost := &tree.Lost{}
	env := toolEnv{dir: t.TempDir(), runner: runner, tree: tree.Container{Runner: runner, Lost: lost}, lost: lost}

	result := shellToolResult(context.Background(), "true", env, maxToolOutputBytes)
	if result["error"] == nil {
		t.Errorf("result = %v; want the runner's error", result)
	}

	if !errors.Is(lost.Taken(), tree.ErrNoAccess) {
		t.Errorf("lost = %v; want the tree marked lost so the attempt ends", lost.Taken())
	}

	hostLost := &tree.Lost{}
	shellToolResult(context.Background(), "true", toolEnv{dir: t.TempDir(), runner: runner, lost: hostLost}, maxToolOutputBytes)

	if hostLost.Taken() != nil {
		t.Errorf("lost = %v; a host runner's failure is not a lost container", hostLost.Taken())
	}
}

func TestTruncationAtExactlyTheLimit(t *testing.T) {
	t.Parallel()

	s := strings.Repeat("a", 16)
	if got := truncateToolOutputLimit(s, len(s)); got != s {
		t.Errorf("truncateToolOutputLimit at exactly the limit = %q; want it unchanged", got)
	}

	if got := spillOrTruncateLimit(s, len(s), t.TempDir()); got != s {
		t.Errorf("spillOrTruncateLimit at exactly the limit = %q; want it inline, not spilled", got)
	}
}

func TestTruncationOfInvalidUTF8NeverCutsBelowZero(t *testing.T) {
	t.Parallel()

	got := truncateToolOutputLimit(strings.Repeat("\x80", 10), 5)
	if !strings.HasPrefix(got, "\n... [truncated 10 bytes]") {
		t.Errorf("got %q; want an empty head and every byte counted as truncated", got)
	}
}
