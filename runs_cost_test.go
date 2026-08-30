package main

// What `steps runs cost` reports, and about which run.
//
// The cost views had no CLI test at all while they were dispatched by flags:
// --run implied --cost, and nothing checked that naming a run reached the
// per-step breakdown rather than the rollup. As a positional the choice is
// the grammar, and this is what proves the grammar picks the right view.

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// costFixture writes a pipeline and records one run with two agent steps in
// the state beside it. Nothing executes: these are rows a run WOULD have
// written, and `steps runs` reads the database, never the YAML.
func costFixture(t *testing.T) string {
	t.Helper()

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: review
  plan:
  - task: compile
    inputs: []
    run: "true"
`)

	st, err := store.OpenStore(statePath(path, ""), pipelineName(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	ctx := context.Background()

	err = st.StartRun(ctx, "REVIEWRUN", "review", t.TempDir())
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for index, name := range []string{"summarize", "critique"} {
		// agent_usage hangs off the node that produced it, so the node has to
		// exist first — the foreign key is what stops spend outliving the
		// cache entry it was recorded against.
		hash := strings.Repeat(string(rune('a'+index)), 64)

		err = st.RecordNode(ctx, store.NodeRecord{
			Hash: hash, Kind: "agent", StepIndex: index, Resource: name,
			Content: map[string]any{"body": name},
		}, "review", "succeeded", nil, nil)
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}

		err = st.RecordAgentUsage(ctx, store.AgentUsage{
			RunID: "REVIEWRUN", StepIndex: index, StepName: name, JobName: "review",
			NodeHash: hash,
			Prompt:   1000, Completion: 500, Total: 1500, Cached: 250,
		})
		if err != nil {
			t.Fatalf("RecordAgentUsage: %v", err)
		}
	}

	err = st.Close()
	if err != nil {
		t.Fatalf("close state store: %v", err)
	}

	return path
}

// TestRunsCostRollsUpByRun is the default view: one line per run, and a
// pointer at the deeper one.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsCostRollsUpByRun(t *testing.T) {
	path := costFixture(t)

	var err error

	out := captureStdout(t, func() { err = run([]string{"runs", "cost", path}) })

	if err != nil {
		t.Fatalf("runs cost: %v", err)
	}

	// The rollup: one row for the run, summing both steps.
	if !strings.Contains(out, "REVIEWRUN") || !strings.Contains(out, "3,000") {
		t.Errorf("rollup does not total the run's two steps:\n%s", out)
	}

	// And not the per-step view, which is what naming a run gets you.
	if strings.Contains(out, "summarize") {
		t.Errorf("rollup printed individual steps:\n%s", out)
	}
}

// TestRunsCostNamesARunForTheBreakdown: the positional is what chooses the
// deeper view. As a flag (--run) this was the only way to reach it, and
// nothing checked that it did.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsCostNamesARunForTheBreakdown(t *testing.T) {
	path := costFixture(t)

	var err error

	out := captureStdout(t, func() { err = run([]string{"runs", "cost", path, "REVIEWRUN"}) })

	if err != nil {
		t.Fatalf("runs cost <run>: %v", err)
	}

	for _, step := range []string{"summarize", "critique"} {
		if !strings.Contains(out, step) {
			t.Errorf("breakdown is missing step %q:\n%s", step, out)
		}
	}
}

// TestRunsCostWillNotVouchForARunItDoesNotHave: agent_usage is
// pipeline-scoped, so a typo — or a run belonging to another pipeline in a
// shared state file — reads back as zero rows. Saying so beats printing an
// empty table that looks like a run which spent nothing.
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsCostWillNotVouchForARunItDoesNotHave(t *testing.T) {
	path := costFixture(t)

	var err error

	out := captureStdout(t, func() { err = run([]string{"runs", "cost", path, "NOSUCHRUN"}) })

	if err != nil {
		t.Fatalf("runs cost <unknown run>: %v", err)
	}

	if !strings.Contains(out, "NOSUCHRUN") || !strings.Contains(out, "no agent usage") {
		t.Errorf("output does not say the run has nothing recorded:\n%s", out)
	}
}

// TestRunsListShowsASuccessfulAgentRun.
//
// The default history view read job_runs — the merkle CACHE index — and
// recordChainSucceeded deliberately skips a chain containing an agent or a
// put, because such a chain is never skippable. So a pipeline of exactly the
// kind steps exists to run recorded every failure and no success: after a run
// that worked, `steps runs list` said "no job runs recorded".
//
// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
func TestRunsListShowsASuccessfulAgentRun(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("done"))

	path := writePipeline(t, dir, `
agents:
- name: writer
  source:
    endpoint: `+fake.URL+`/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

defaults:
  preflight:
    disabled: true

jobs:
- name: review
  plan:
  - agent: writer
    inputs: []
    messages:
      - Say something.
`)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, "run", path, "--job", "review")

	var err error

	out := captureStdout(t, func() { err = run([]string{"runs", "list", path}) })

	if err != nil {
		t.Fatalf("runs list: %v", err)
	}

	if !strings.Contains(out, "succeeded") || !strings.Contains(out, "review") {
		t.Errorf("a successful agent run is missing from the default history view:\n%s", out)
	}

	// And the row carries the run id, which is what makes the deeper views
	// reachable from this one.
	if !strings.Contains(out, "steps runs steps") {
		t.Errorf("output does not point at where the per-step detail is:\n%s", out)
	}
}
