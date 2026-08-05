package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// End-to-end tests: one pipeline, one scripted provider, assertions at every
// layer the run passes through. Unlike the per-package unit tests — which
// each verify one layer against a stub of its neighbor — these prove the
// layers agree with each other on a single real run: the resource fetch the
// agent actually reads, the tool call the agent actually executes, the
// artifact that actually reaches the put, the route the verdict actually
// takes, and the rows the store actually records.
//
// Neither test uses t.Parallel(): both call t.Setenv for the agent's API key
// env var, which panics once a parallel test has started.

// e2ePipeline renders the fixture both end-to-end tests share. It is one
// pipeline exercising every step kind and the agent's full conversation
// surface:
//
//	get repo      -> a dummy resource whose in: writes NOTES.txt
//	task prepare  -> produces a second artifact for the agent to consume
//	agent reviewer-> reads NOTES.txt, writes report/summary.md, emits a verdict
//	task escalate -> the reject branch; fails, so the job fails and the put is
//	                 never reached
//	put results   -> the approve branch; its out: reads the agent's artifact
//
// The verdict routes forward past escalate on approve, and onto it on
// reject — so the two branches are distinguishable purely by which log files
// exist afterward. Every step's side effect is an append to a log file under
// dir (absolute paths, since each command runs in its own workspace), because
// step workspaces are torn down when the build closes and are not
// inspectable after the fact.
//
// workspaceBlock is prepended verbatim: "" for the default shared directory,
// or a workspace: block to run the same fixture under real isolation. The
// inputs:/outputs: declarations below are a validated contract either way,
// but only under isolation do they physically scope what each step sees —
// which is what makes the artifact-flow assertions load-bearing rather than
// incidentally true (see docs/workspace.md).
func e2ePipeline(t *testing.T, dir, endpoint, workspaceBlock string) string {
	t.Helper()

	yaml := workspaceBlock + fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: |
      echo 'The widget catalog is seeded from widgets.json on boot.' > NOTES.txt
      echo fetched >> %[2]s
    out: |
      cat report/summary.md >> %[5]s

resources:
- name: repo
  type: dummy
  source: {}
- name: results
  type: dummy
  source: {}

agents:
- name: reviewer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file]

jobs:
- name: build
  plan:
  - get: repo
  - task: prepare
    inputs: [repo]
    outputs: [prep]
    run: |
      mkdir -p prep
      echo ready-for-review > prep/STATUS.txt
      echo ran >> %[3]s
  - agent: reviewer
    inputs: [repo, prep]
    outputs: [report]
    prompt: Review the notes and summarize them.
    verdicts: [approve, reject]
    to:
      approve: results
      reject: escalate
      failure: escalate
    assert:
      tool_calls:
      - name: read_file
        args: { path: repo/NOTES.txt }
      - name: write_file
  - task: escalate
    inputs: []
    run: |
      echo escalated >> %[4]s
      exit 1
  - put: results
    inputs: [report]
`, endpoint, filepath.Join(dir, "get.log"), filepath.Join(dir, "task.log"),
		filepath.Join(dir, "escalate.log"), filepath.Join(dir, "put.log"))

	return writePipeline(t, dir, yaml)
}

// happyPathScript is the conversation the fake provider plays back on the
// approve branch. Two turns are deliberately shaped: turn 1 requests both
// reads at once (the parallel-tool-call form), which is how the agent's two
// declared input artifacts get proven materialized in a single round trip;
// turn 3 tries to finish with plain text while the synthesized verdict tool
// is still unsatisfied, which is what makes the conversation loop force
// tool_choice on turn 4 — a middle layer with no other end-to-end coverage.
func happyPathScript() []turn {
	return []turn{
		callsTools(
			call("read_file", map[string]any{"path": "repo/NOTES.txt"}),
			call("read_file", map[string]any{"path": "prep/STATUS.txt"}),
		),
		callsTool("write_file", map[string]any{
			"path":    "report/summary.md",
			"content": "widgetd seeds its catalog from widgets.json.",
		}),
		says("I have finished reviewing."),
		callsTool("verdict", map[string]any{"choice": "approve", "note": "notes are accurate"}),
		says("Approved."),
	}
}

// TestEndToEndAgentHappyPath drives a full approve-branch run and asserts at
// each layer it passes through, in order: resource fetch, task execution,
// tool compilation on the wire, tool execution, required-tool forcing,
// verdict routing, artifact flow into the put, and store persistence. It
// finishes by rerunning the pipeline to pin down the caching contract.
//
// It runs the identical fixture under both workspace modes. Shared is the
// default every pipeline gets; isolated is the mode in which the
// artifact-flow assertions actually bite — under shared mode every step sees
// one mutable directory, so a file reaching the put proves nothing about
// output capture (verified: deleting the agent step's declared outputs from
// its TaskSpace call leaves the shared run passing and fails only the
// isolated one).
func TestEndToEndAgentHappyPath(t *testing.T) {
	for _, mode := range []struct{ name, workspace string }{
		{"shared workspace", ""},
		{"isolated workspace", "workspace:\n  strategy: copy\n"},
	} {
		t.Run(mode.name, func(t *testing.T) {
			runHappyPath(t, mode.workspace)
		})
	}
}

func runHappyPath(t *testing.T, workspaceBlock string) {
	t.Helper()

	dir := t.TempDir()
	fake := newFakeLLM(t, happyPathScript()...)
	path := e2ePipeline(t, dir, fake.URL, workspaceBlock)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// ── resource layer ────────────────────────────────────────────────────
	// The get's in: ran exactly once and produced the artifact the rest of
	// the run consumes.
	assertLineCount(t, filepath.Join(dir, "get.log"), 1)

	// ── task layer ────────────────────────────────────────────────────────
	assertLineCount(t, filepath.Join(dir, "task.log"), 1)

	// ── wire + tool-execution layers ──────────────────────────────────────
	assertHappyPathConversation(t, fake)

	// ── routing layer ─────────────────────────────────────────────────────
	// approve routed forward past escalate, so the reject branch never ran.
	assertNoFile(t, filepath.Join(dir, "escalate.log"))

	// ── artifact-flow layer ───────────────────────────────────────────────
	// write_file landed in the agent's declared report output, which was
	// materialized into the put's input view and read by the resource's out:.
	if got := readFileString(t, filepath.Join(dir, "put.log")); !strings.Contains(got, "widgetd seeds its catalog") {
		t.Errorf("put's out: did not see the agent's report artifact; got %q", got)
	}

	// ── store layer ───────────────────────────────────────────────────────
	nodes := storeNodes(t, path)

	assertSucceeded(t, nodes, "get", "repo")
	assertSucceeded(t, nodes, "task", "prepare")
	assertSucceeded(t, nodes, "agent", "reviewer")
	assertSucceeded(t, nodes, "put", "results")

	// The routed-past escalate step records no node at all — the same
	// contract a cached or when:-skipped step has.
	for _, node := range nodes {
		if node.Resource == "escalate" {
			t.Errorf("routed-past step recorded a node: %+v", node)
		}
	}

	// ── caching layer ─────────────────────────────────────────────────────
	// A chain containing a put or an agent is never skippable, so a fully
	// successful run records no reusable succeeded chain at all — job_runs
	// stays empty. (A *failing* run still records a row, carrying its
	// classification; see the sad-path test.)
	if runs := storeJobRuns(t, path); len(runs) != 0 {
		t.Errorf("job_runs = %+v, want none (a succeeded chain containing a put/agent is never recorded)", runs)
	}

	// With nothing recorded to skip against, the whole plan re-executes on a
	// rerun — whole-chain granularity, not per-step: the get and the task
	// rerun even though their own content is unchanged.

	fake2 := newFakeLLM(t, happyPathScript()...)
	rerunPath := e2ePipeline(t, dir, fake2.URL, workspaceBlock)

	mustRun(t, rerunPath)

	assertLineCount(t, filepath.Join(dir, "get.log"), 2)
	assertLineCount(t, filepath.Join(dir, "task.log"), 2)

	if got := fake2.requestCount(); got != len(happyPathScript()) {
		t.Errorf("provider requests on rerun = %d, want %d (agent steps are never skip-cached)", got, len(happyPathScript()))
	}
}

// assertHappyPathConversation checks everything observable on the wire: what
// the agent step sent, what its tool executions fed back, and how the loop
// reacted to the model trying to stop early.
func assertHappyPathConversation(t *testing.T, fake *fakeLLM) {
	t.Helper()

	if got := fake.requestCount(); got != len(happyPathScript()) {
		t.Fatalf("provider requests = %d, want %d (one per scripted turn)", got, len(happyPathScript()))
	}

	assertRequestShape(t, fake.request(1))
	assertToolFlow(t, fake)
}

// assertRequestShape checks the opening request: the resolved model, the
// compiled tool set, the assembled system message, and the step's prompt.
func assertRequestShape(t *testing.T, first capturedRequest) {
	t.Helper()

	if first.Model != "test-model" {
		t.Errorf("request 1 model = %q, want test-model", first.Model)
	}

	// The tools offered are the agent's grant plus the verdict tool
	// synthesized from verdicts:. Both halves reaching the wire is the proof
	// that tool compilation ran against the resolved agent config.
	wantTools := []string{"read_file", "write_file", "verdict"}
	if got := first.toolNames(); !slices.Equal(got, wantTools) {
		t.Errorf("request 1 offered tools = %v, want %v", got, wantTools)
	}

	// The system message carries the persona plus the operating note naming
	// the step's own working directory.
	if sys := first.systemMessage(); !strings.Contains(sys, "Your working directory is ") {
		t.Errorf("request 1 system message is missing the operating note; got %q", sys)
	}

	if len(first.Messages) != 2 || first.Messages[1].Content != "Review the notes and summarize them." {
		t.Errorf("request 1 did not carry the step's prompt as the user message; got %+v", first.Messages)
	}

	if got := first.forcedTool(); got != "" {
		t.Errorf("request 1 forced %q; nothing should be forced before the model tries to stop", got)
	}
}

// assertToolFlow checks what the agent's tool executions fed back to the
// model, and that the loop forced the unsatisfied required tool when the
// model tried to stop without it.
func assertToolFlow(t *testing.T, fake *fakeLLM) {
	t.Helper()

	// Both reads ran and both results were fed back, in call order. That the
	// first carries the get's fetched text and the second the prepare task's
	// output is the end-to-end proof of artifact materialization: two
	// artifacts produced by two different step kinds, both present in the
	// agent's own working directory under their declared names.
	results := fake.request(2).toolResults()
	if len(results) != 2 {
		t.Fatalf("request 2 carried %d tool results, want 2 (one per parallel call); got %v", len(results), results)
	}

	if !strings.Contains(results[0], "seeded from widgets.json") {
		t.Errorf("read_file(repo/NOTES.txt) did not return the get's fetched artifact; got %q", results[0])
	}

	if !strings.Contains(results[1], "ready-for-review") {
		t.Errorf("read_file(prep/STATUS.txt) did not return the prepare task's output artifact; got %q", results[1])
	}

	// Turn 3 replied with text while verdict was still unsatisfied, so the
	// next request must force it rather than accept the answer.
	if got := fake.request(4).forcedTool(); got != "verdict" {
		t.Errorf("request 4 tool_choice forced %q, want verdict", got)
	}
}

// assertSucceeded fails the test unless the named node was recorded as
// succeeded.
func assertSucceeded(t *testing.T, nodes []nodeRow, kind, resource string) {
	t.Helper()

	node := findNode(t, nodes, kind, resource)
	if node.Status != "succeeded" {
		t.Errorf("%s node %q status = %q (error %q), want succeeded", kind, resource, node.Status, node.Error)
	}
}

// TestEndToEndAgentSadPath covers the two failure modes that take genuinely
// different paths through outcome classification. They are separate subtests
// rather than one because the distinction between them — a model that
// decides "no" is a routable task-level failure, an unreachable provider is
// not routable at all — is a load-bearing design invariant of route.go's
// outcomeKey, and nothing else tests it end to end.
func TestEndToEndAgentSadPath(t *testing.T) {
	t.Run("model rejects", testSadPathModelRejects)
	t.Run("provider unreachable", testSadPathProviderUnreachable)
}

// testSadPathModelRejects: the model reaches a decision and it is "no". That
// is a routable outcome — the reject verdict takes the escalate branch, which
// fails the job — and everything upstream of it still succeeded.
func testSadPathModelRejects(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("read_file", map[string]any{"path": "repo/NOTES.txt"}),
		callsTool("write_file", map[string]any{
			"path":    "report/summary.md",
			"content": "The notes overstate what the daemon persists.",
		}),
		callsTool("verdict", map[string]any{"choice": "reject", "note": "claims are unsupported"}),
		says("Rejected."),
	)
	path := e2ePipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("run succeeded; the reject branch's escalate step exits 1, so the job must fail")
	}

	// ── routing layer ─────────────────────────────────────────────────
	// The verdict is a routing key, so reject took the escalate branch...
	assertLineCount(t, filepath.Join(dir, "escalate.log"), 1)

	// ...and the put, which only the approve branch reaches, never ran.
	assertNoFile(t, filepath.Join(dir, "put.log"))

	// ── store layer ───────────────────────────────────────────────────
	// Partial progress is recorded honestly: the steps that succeeded
	// before the failure are still marked succeeded, and the agent step
	// itself succeeded — it did its job by deciding "no". Only the
	// escalate task failed.
	nodes := storeNodes(t, path)

	for _, want := range []struct{ kind, resource string }{
		{"get", "repo"}, {"task", "prepare"}, {"agent", "reviewer"},
	} {
		node := findNode(t, nodes, want.kind, want.resource)
		if node.Status != "succeeded" {
			t.Errorf("%s node %q status = %q, want succeeded", want.kind, want.resource, node.Status)
		}
	}

	failed := findNode(t, nodes, "task", "escalate")
	if failed.Status != "failed" {
		t.Errorf("escalate node status = %q, want failed", failed.Status)
	}

	if failed.Error == "" {
		t.Error("escalate node recorded no error message")
	}

	for _, node := range nodes {
		if node.Kind == "put" {
			t.Errorf("a put node was recorded on the reject branch: %+v", node)
		}
	}

	// The chain's job_runs row carries the classification, not just
	// "didn't work": a task exiting nonzero is a task-level *failure*.
	// Compare with the provider-outage subtest below, which records the
	// same shape with status errored — that difference is the whole
	// point of outcome.Classify, and this is where it lands durably.
	runs := storeJobRuns(t, path)
	if len(runs) != 1 || runs[0].Status != "failed" {
		t.Errorf("job_runs = %+v, want exactly one row with status failed", runs)
	}
}

// testSadPathProviderUnreachable: the model never reaches a decision at all
// because the endpoint is down. That is infrastructure, not a verdict — so it
// classifies as errored, produces no routing key, and the step's to.failure
// target must stay untouched.
func testSadPathProviderUnreachable(t *testing.T) {
	dir := t.TempDir()

	// Every turn 500s. The script is sized well past the retry budget so
	// the harness never reports exhaustion for what is a deliberate
	// outage; the assertion below pins how many requests were actually
	// made.
	outage := make([]turn, 10)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	fake := newFakeLLM(t, outage...)
	path := e2ePipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	logs := captureStderr(t)

	err := run([]string{path})
	if err == nil {
		t.Fatal("run succeeded despite the provider being unreachable")
	}

	stderr := logs()

	// ── resource/task layers still ran ────────────────────────────────
	// The failure is isolated to the agent step; everything upstream of
	// it completed normally.
	assertLineCount(t, filepath.Join(dir, "get.log"), 1)
	assertLineCount(t, filepath.Join(dir, "task.log"), 1)

	// ── retry layer ───────────────────────────────────────────────────
	// One failing agent step costs three provider requests, not one.
	// That amplification is invisible in this repo's own code: the step
	// declares no attempts:, and internal/retry logs a single attempt —
	// the extra two come from the underlying client's own transport
	// retry (openai-go/v3 defaults to MaxRetries: 2). Pinned here
	// because it silently multiplies latency and spend on every failing
	// call; if a dependency bump changes it, this is where you find out.
	const wantRequests = 3

	if got := fake.requestCount(); got != wantRequests {
		t.Errorf("provider requests = %d, want %d (1 attempt + the client's 2 transport retries)", got, wantRequests)
	}

	assertHiddenRetriesVisible(t, stderr, wantRequests)

	// ── routing layer ─────────────────────────────────────────────────
	// This is the invariant that separates this subtest from the one
	// above: an errored step produces no routing key, so the step's
	// to.failure target must NOT fire. A provider outage is not a
	// verdict, and routing on it would let an outage masquerade as a
	// decision the model never made.
	assertNoFile(t, filepath.Join(dir, "escalate.log"))
	assertNoFile(t, filepath.Join(dir, "put.log"))

	// ── store layer ───────────────────────────────────────────────────
	nodes := storeNodes(t, path)

	// The classification reaches the store intact: errored, not failed.
	// Everything upstream of the agent still records succeeded, so a
	// later run can tell how far it got.
	agentNode := findNode(t, nodes, "agent", "reviewer")
	if agentNode.Status != "errored" {
		t.Errorf("agent node status = %q, want errored (an unreachable provider is infrastructure, not a task-level failure)", agentNode.Status)
	}

	for _, want := range []struct{ kind, resource string }{{"get", "repo"}, {"task", "prepare"}} {
		node := findNode(t, nodes, want.kind, want.resource)
		if node.Status != "succeeded" {
			t.Errorf("%s node %q status = %q, want succeeded", want.kind, want.resource, node.Status)
		}
	}

	runs := storeJobRuns(t, path)
	if len(runs) != 1 || runs[0].Status != "errored" {
		t.Errorf("job_runs = %+v, want exactly one row with status errored", runs)
	}
}

// TestEndToEndTryToleratesFailure exercises the try: wrapper: a failing task
// inside a try block does not fail the plan — the plan continues with the
// next step, and the store records the try node as succeeded while the inner
// step's node records its real outcome.
func TestEndToEndTryToleratesFailure(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "try-after.log")

	pipeline := fmt.Sprintf(`
jobs:
- name: try-demo
  plan:
  - try:
      task: flaky
      run: exit 1
  - task: still-runs
    run: echo after-try >> %s
`, logPath)

	path := writePipeline(t, dir, pipeline)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run should have succeeded (try swallowed the failure): %v", err)
	}

	// The plan continued past the failed tried step.
	assertLineCount(t, logPath, 1)

	nodes := storeNodes(t, path)

	// The try wrapper node is recorded as succeeded.
	tryNode := findNode(t, nodes, "try", "flaky")
	if tryNode.Status != "succeeded" {
		t.Errorf("try node status = %q, want succeeded", tryNode.Status)
	}

	// The inner task node is recorded with its real failed outcome.
	failedTask := findNode(t, nodes, "task", "flaky")
	if failedTask.Status != "failed" {
		t.Errorf("inner task node status = %q, want failed", failedTask.Status)
	}

	// The step after try still ran.
	findNode(t, nodes, "task", "still-runs")
}

// TestEndToEndTryRoutesOnRealOutcome pins the half of try: that is easiest to
// get backwards: the wrapper is transparent to everything that OBSERVES the
// outcome, and only the plan walker is lied to. A `to: {failure: ...}` on the
// wrapper must fire (an implementation that swallowed the error before routing
// made every failure branch dead code and silently fell through to the next
// step instead), and the wrapped step's own hooks must fire on its real result.
func TestEndToEndTryRoutesOnRealOutcome(t *testing.T) {
	dir := t.TempDir()
	hookLog := filepath.Join(dir, "hook.log")
	recoverLog := filepath.Join(dir, "recover.log")
	fallthroughLog := filepath.Join(dir, "fallthrough.log")

	pipeline := fmt.Sprintf(`
jobs:
- name: try-demo
  plan:
  - try:
      task: migrate
      run: exit 1
      on_failure:
        task: alert
        run: echo alerted >> %[1]s
    to:
      failure: recover
      success: deploy
  - task: deploy
    run: echo deployed >> %[3]s
  - task: recover
    run: echo recovered >> %[2]s
`, hookLog, recoverLog, fallthroughLog)

	path := writePipeline(t, dir, pipeline)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run should have succeeded (the failure routed): %v", err)
	}

	// The wrapped step's own on_failure hook saw the real (failed) outcome.
	assertLineCount(t, hookLog, 1)

	// to: failure won: the plan jumped to recover instead of falling through.
	assertLineCount(t, recoverLog, 1)

	// deploy is the fall-through target the failure route must have skipped.
	assertNoFile(t, fallthroughLog)
}

// TestEndToEndTryDoesNotTolerateInfraError pins the other line: try: swallows a
// task-level failure and nothing else. An unreachable provider is an
// infrastructure error, so a try-wrapped agent that can't be reached must still
// fail the run — swallowing it would report a green job for an outage, and the
// same classification is what keeps a Ctrl-C from being reported as success.
func TestEndToEndTryDoesNotTolerateInfraError(t *testing.T) {
	dir := t.TempDir()
	afterLog := filepath.Join(dir, "after.log")

	pipeline := fmt.Sprintf(`
agents:
- name: reviewer
  source:
    endpoint: http://127.0.0.1:1/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: try-demo
  plan:
  - try:
      agent: reviewer
      prompt: Review it.
      attempts: 1
  - task: after
    run: echo after >> %s
`, afterLog)

	path := writePipeline(t, dir, pipeline)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := run([]string{path})
	if err == nil {
		t.Fatal("run should have failed: try: does not tolerate an infrastructure error")
	}

	// The plan must not have continued past an untolerated error.
	assertNoFile(t, afterLog)
}

// assertHiddenRetriesVisible checks the run reported the transport-layer
// retries it actually made. It is the end-to-end half of the unit tests in
// internal/agent/requests_test.go: those prove the transport counts correctly
// in isolation, this proves it is in the client's stack at all and that the
// per-attempt counter reaches internal/retry.
//
// One line short of wantRequests, because the last failure of a burst is not
// followed by a retry — retry.attempt_failed reports that one, with the full
// request count beside it.
func assertHiddenRetriesVisible(t *testing.T, stderr string, wantRequests int) {
	t.Helper()

	if got := strings.Count(stderr, "agent.request_retry"); got != wantRequests-1 {
		t.Errorf("agent.request_retry lines = %d, want %d\n%s", got, wantRequests-1, stderr)
	}

	want := fmt.Sprintf("provider_requests=%d", wantRequests)
	if !strings.Contains(stderr, want) {
		t.Errorf("retry.attempt_failed does not report what the attempt really cost (want %q):\n%s", want, stderr)
	}
}
