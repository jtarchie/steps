package e2e

// A hook's agent and a task's fix: agent spend real tokens inside a job, and
// neither has a merkle node. agent_usage used to require one, so their rows
// were never written: `steps runs cost` under-reported the run, and a resumed
// run seeded its job budget from a ledger missing that spend.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestRunsCostIncludesAHookAgentsSpend: an on_success: agent's tokens show up
// in both the rollup and the per-step breakdown, marked as a hook.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsCostIncludesAHookAgentsSpend(t *testing.T) {
	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("noted").spending(7000))

	path := hookAgentPipeline(t, dir, fake.URL)

	mustRun(t, "run", path, "--job", "build")

	assertRunCostShows(t, path, "notifier (hook)", "7,000", 7000)
}

// hookAgentPipeline is one task whose on_success: hook is an agent.
func hookAgentPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: notifier
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - task: work
    inputs: []
    run: echo did the work
    on_success:
      agent: notifier
      inputs: []
      messages:
        - Say something.
`, endpoint))
}

// TestRunPageDrawsAHookAgentsSpendOnItsRow: the spend the ledger records for a hook's agent is drawn on the hook's own row of the run page, not dropped for want of a node.
func TestRunPageDrawsAHookAgentsSpendOnItsRow(t *testing.T) {
	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("noted").spending(7000))
	path := hookAgentPipeline(t, dir, fake.URL)

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	if hook := headOf(t, body, "hook"); !strings.Contains(hook, "7,000 tokens") {
		t.Errorf("the hook's row does not show its agent's 7,000 tokens:\n%s", hook)
	}

	if task := headOf(t, body, "task"); strings.Contains(task, "7,000 tokens") {
		t.Errorf("the hook's spend is drawn on the step it guards:\n%s", task)
	}
}

// TestRunPageDrawsAFixAgentsSpendOnItsRow: a fix: agent's spend is drawn on the row of the task it repaired.
func TestRunPageDrawsAFixAgentsSpendOnItsRow(t *testing.T) {
	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("done").spending(9000))
	path := writeFixPipeline(t, dir, fake.URL, failThenPass(filepath.Join(dir, "counter.txt")))

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	if task := headOf(t, body, "task"); !strings.Contains(task, "9,000 tokens") {
		t.Errorf("the repaired task's row does not show its fix agent's 9,000 tokens:\n%s", task)
	}
}

// TestRunsCostIncludesAFixAgentsSpend: a fix: agent's tokens are the job's
// spend too, and are reported as the fix rather than as a plan step.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsCostIncludesAFixAgentsSpend(t *testing.T) {
	dir := t.TempDir()
	fake := newRepeatingFakeLLM(t, says("done").spending(9000))

	path := writeFixPipeline(t, dir, fake.URL, failThenPass(filepath.Join(dir, "counter.txt")))

	mustRun(t, "run", path, "--job", "build")

	assertRunCostShows(t, path, "fixer (fix)", "9,000", 9000)
}

// assertRunCostShows runs `steps runs cost` for the pipeline's only run, then
// the breakdown of that run, and checks both carry the node-less agent's
// tokens.
func assertRunCostShows(t *testing.T, path, step, tokens string, want int) {
	t.Helper()

	var err error

	rollup := captureStdout(t, func() { err = cli.Run(append([]string{"runs", "cost"}, readArgs(path)...)) })
	if err != nil {
		t.Fatalf("runs cost: %v", err)
	}

	if !strings.Contains(rollup, tokens) {
		t.Fatalf("rollup does not count the %s's %s tokens:\n%s", step, tokens, rollup)
	}

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	spent, err := st.RunTokensSpent(t.Context(), runs[0].ID)
	if err != nil {
		t.Fatalf("RunTokensSpent: %v", err)
	}

	// What a resume seeds the job budget from.
	if spent != want {
		t.Errorf("RunTokensSpent = %d, want %d — a resumed run would not count the %s's spend against the job budget", spent, want, step)
	}

	breakdown := captureStdout(t, func() {
		err = cli.Run(append([]string{"runs", "cost", runs[0].ID}, readArgs(path)...))
	})
	if err != nil {
		t.Fatalf("runs cost %s: %v", runs[0].ID, err)
	}

	if !strings.Contains(breakdown, step) || !strings.Contains(breakdown, tokens) {
		t.Errorf("breakdown does not list %q with %s tokens:\n%s", step, tokens, breakdown)
	}
}
