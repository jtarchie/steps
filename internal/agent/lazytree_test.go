package agent

// When the step's container is allowed to start.

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// countingRunner records whether anything was ever run through it, which is the whole question: probing an image means running a real command in its container, and for a placed agent that means dialling the worker and sending it the tree.
type countingRunner struct {
	shell.Runner
	commands []string
}

func (c *countingRunner) RunCaptureFullLimited(_ context.Context, command string, _ int, _ string) (string, string, int, error) {
	c.commands = append(c.commands, command)

	return "pwd=/work\n", "", 0, nil
}

func (c *countingRunner) WithLabel(string) shell.Runner { return c }

func (c *countingRunner) Close() error { return nil }

// TestPreparingAContainerAgentStartsNothing is the step cache's promise stated where it can be checked.
//
// RunStep decides a cache hit AFTER preparing, so a probe run during preparation is paid by every reuse of a containerized agent — a container started and thrown away, and for a placed one a worker dialled and a tree uploaded, for a conversation that never happens. lookupStepCache's own doc puts the cost of a hit at one materialized workspace and whatever tool servers the grant starts; this keeps that true.
func TestPreparingAContainerAgentStartsNothing(t *testing.T) {
	t.Parallel()

	runner := &countingRunner{}

	prepared := preparedAgentStep{
		dir:    t.TempDir(),
		runner: runner,
		lost:   &lostTree{},
		ri: config.ResolvedInvocation{
			AgentName: "hand",
			Image:     "alpine:3",
			ToolSpecs: []config.ToolSpec{{Builtin: "read_file"}},
		},
		space: testStepSpace(t),
	}

	// Standing in for what prepareAgentStep leaves behind: a conversation whose tree is not yet settled.
	if prepared.conv.env.tree != nil {
		t.Fatal("the fixture already carries a tree, so this asserts nothing")
	}

	if len(runner.commands) != 0 {
		t.Errorf("preparing ran %v in the step's container; a cached step pays for every one of those", runner.commands)
	}

	prepared, err := prepared.openTree(context.Background())
	if err != nil {
		t.Fatalf("openTree: %v", err)
	}

	if len(runner.commands) == 0 {
		t.Error("openTree ran nothing, so the probe that reports the container's working directory never happened")
	}

	if prepared.conv.env.tree == nil {
		t.Error("openTree left the file tools pointed at this machine for an agent with image:")
	}

	if prepared.conv.system == "" {
		t.Error("openTree left the conversation with no system message")
	}
}

// TestPreparingAHostAgentNeedsNoProbe keeps the laziness from costing the common case anything: with no image: there is no container to ask, so openTree must not run a command at all.
func TestPreparingAHostAgentNeedsNoProbe(t *testing.T) {
	t.Parallel()

	runner := &countingRunner{}
	dir := t.TempDir()

	prepared := preparedAgentStep{
		dir:    dir,
		runner: runner,
		lost:   &lostTree{},
		ri: config.ResolvedInvocation{
			AgentName: "hand",
			ToolSpecs: []config.ToolSpec{{Builtin: "read_file"}},
		},
		space: testStepSpace(t),
	}

	prepared, err := prepared.openTree(context.Background())
	if err != nil {
		t.Fatalf("openTree: %v", err)
	}

	if len(runner.commands) != 0 {
		t.Errorf("an agent with no image: ran %v; there is no container to ask", runner.commands)
	}

	if host, ok := prepared.conv.env.tree.(hostTree); !ok || host.dir != dir {
		t.Errorf("tree = %#v, want this machine's own rooted at %q", prepared.conv.env.tree, dir)
	}
}
