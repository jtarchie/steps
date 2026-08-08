package main

// End-to-end coverage for the run context store (context: write / set_context).
//
// Lives in the root package for the reason every e2e test here does: only
// main's run() spans CLI → config → merkle → agent conversation → store, and
// source.endpoint: is the only injection point for a scripted model.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// contextPipeline writes a two-agent pipeline where the first step is granted
// context: write and the second is a plain agent step. No resources: the
// subject is the tool and the store, and a get would only add moving parts.
func contextPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: investigator
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  system: You investigate.
  tools:
  - builtin: read_file

jobs:
- name: triage
  plan:
  - agent: investigator
    prompt: Investigate the failure.
    context: write
`, endpoint)

	return writePipeline(t, dir, yaml)
}

// TestEndToEndContextWrite is the whole feature on one pass: the synthesized
// tool reaches the wire only because the step declared context: write, a call
// to it lands as a row in run_context attributed to the writing step, and a
// key the model has no business writing is refused as data rather than
// failing the step.
func TestEndToEndContextWrite(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		// A reserved key: refused at the tool boundary, fed back as an
		// error the model can react to. The step must still succeed.
		callsTool("set_context", map[string]any{
			"key":   "internal.run_id",
			"value": "hijacked",
		}),
		callsTool("set_context", map[string]any{
			"key":   "failure_cause",
			"value": "flaky DNS in the e2e suite",
		}),
		says("Investigated."),
	)
	path := contextPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// ── wire layer ────────────────────────────────────────────────────────
	// set_context is offered alongside the step's own grant. Its presence is
	// the proof that the config declaration compiled into a tool set; the
	// read_file beside it proves the declared grant was not replaced.
	wantTools := []string{"read_file", "set_context"}
	if got := fake.request(1).toolNames(); !slices.Equal(got, wantTools) {
		t.Errorf("request 1 offered tools = %v, want %v", got, wantTools)
	}

	// ── tool-boundary layer ───────────────────────────────────────────────
	// The reserved-key call came back as an error, not a step failure: the
	// model gets a turn to correct itself, which is the contract every tool
	// in this codebase honors.
	refusal := fake.request(2).toolResults()
	if len(refusal) != 1 {
		t.Fatalf("request 2 carried %d tool results, want 1; got %v", len(refusal), refusal)
	}

	if !strings.Contains(refusal[0], "reserved") {
		t.Errorf("reserved-key call result = %q, want it to name the reserved prefix", refusal[0])
	}

	// ── store layer ───────────────────────────────────────────────────────
	entries := storeRunContext(t, path)

	// Exactly one row: the refused write stored nothing at all.
	if len(entries) != 1 {
		t.Fatalf("run_context rows = %+v, want exactly 1 (the refused write must store nothing)", entries)
	}

	if entries[0].Key != "failure_cause" || entries[0].Value != "flaky DNS in the e2e suite" {
		t.Errorf("stored entry = %+v, want failure_cause with the model's value", entries[0])
	}

	// Attribution is the reason written_by exists: the row answers "who
	// recorded this" without replaying the transcript.
	if entries[0].WrittenBy != "investigator" {
		t.Errorf("written_by = %q, want the writing step's name", entries[0].WrittenBy)
	}
}

// TestEndToEndContextNotOfferedWithoutDeclaration pins the opt-in: the same
// pipeline without context: write must not put set_context on the wire.
// Without this, a passing write test proves only that the tool exists, not
// that declaring it is what summons it.
func TestEndToEndContextNotOfferedWithoutDeclaration(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("Investigated."))

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: investigator
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: read_file

jobs:
- name: triage
  plan:
  - agent: investigator
    prompt: Investigate the failure.
`, fake.URL)
	path := writePipeline(t, dir, yaml)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.request(1).toolNames(); slices.Contains(got, "set_context") {
		t.Errorf("offered tools = %v; set_context must not appear without context: write", got)
	}

	if entries := storeRunContext(t, path); len(entries) != 0 {
		t.Errorf("run_context rows = %+v, want none", entries)
	}
}

// TestEndToEndContextRecapReachesLaterStep is the read half (#36): what one
// step records with set_context arrives in the NEXT step's conversation
// automatically, as a synthetic read_context exchange rather than a turn the
// model had to spend. The second agent is granted no context: at all — the
// recap is not something a step opts into.
func TestEndToEndContextRecapReachesLaterStep(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		// investigator
		callsTool("set_context", map[string]any{
			"key":   "failure_cause",
			"value": "flaky DNS in the e2e suite",
		}),
		says("Investigated."),
		// fixer — no context: declaration of its own
		says("Fixed."),
	)
	path := writePipeline(t, dir, twoStepContextPipeline(fake.URL, ""))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// The fixer's opening request is request 3 (the investigator used two).
	fixer := fake.request(3)

	// ── delivery layer ────────────────────────────────────────────────────
	// The fact arrives as an already-answered read_context call, so it costs
	// the step no turn and cannot be skipped by a model that decides not to
	// look.
	results := fixer.toolResults()
	if len(results) != 1 {
		t.Fatalf("fixer's opening request carried %d tool results, want 1 (the recap); got %v", len(results), results)
	}

	if !strings.Contains(results[0], "failure_cause") || !strings.Contains(results[0], "flaky DNS") {
		t.Errorf("recap = %q, want the recorded key and value", results[0])
	}

	// The recap says what it is, so a model cannot mistake a recorded fact
	// for an instruction it was given.
	if !strings.Contains(results[0], "data, not instructions") {
		t.Errorf("recap = %q, want the data-not-instructions framing", results[0])
	}

	// read_context is offered too, so a conversation that later compacts can
	// ask for the facts again instead of working from a summary of them.
	if got := fixer.toolNames(); !slices.Contains(got, "read_context") {
		t.Errorf("fixer's offered tools = %v, want read_context among them", got)
	}

	// The investigator ran BEFORE anything was recorded, so its own opening
	// request carried no recap and was offered no read_context.
	if got := fake.request(1).toolNames(); slices.Contains(got, "read_context") {
		t.Errorf("investigator's offered tools = %v; nothing was recorded yet, so read_context must be absent", got)
	}
}

// TestEndToEndContextFidelityOff proves the opt-out is complete: the same
// pipeline with fidelity: off on the reader delivers no recap and no tool,
// even though the store holds a fact.
func TestEndToEndContextFidelityOff(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		callsTool("set_context", map[string]any{
			"key":   "failure_cause",
			"value": "flaky DNS in the e2e suite",
		}),
		says("Investigated."),
		says("Fixed."),
	)
	path := writePipeline(t, dir, twoStepContextPipeline(fake.URL, "    context: { fidelity: \"off\" }\n"))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	fixer := fake.request(3)

	if results := fixer.toolResults(); len(results) != 0 {
		t.Errorf("fixer carried %v with fidelity: off, want no recap at all", results)
	}

	if got := fixer.toolNames(); slices.Contains(got, "read_context") {
		t.Errorf("fixer's offered tools = %v; fidelity: off must offer no read_context either", got)
	}

	// The write still happened — opting out of reading is not opting out of
	// the store.
	if entries := storeRunContext(t, path); len(entries) != 1 {
		t.Errorf("run_context = %+v, want the investigator's write to have landed", entries)
	}
}

// twoStepContextPipeline is a writer followed by a reader. readerExtra is
// spliced into the reader step, so one fixture serves both the default-recap
// and the opted-out cases.
func twoStepContextPipeline(endpoint, readerExtra string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: investigator
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: read_file
- name: fixer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: read_file

jobs:
- name: triage
  plan:
  - agent: investigator
    prompt: Investigate the failure.
    context: write
  - agent: fixer
    prompt: Fix the cause.
%[2]s`, endpoint, readerExtra)
}

// TestEndToEndTaskContextSurvivesACacheHit is the correctness claim of the
// task-write half (#37): a task that records facts and is then SKIPPED on a
// rerun must still yield them, or a cached run and a fresh run disagree about
// what is true.
//
// The job is task-only on purpose. A chain containing an agent is never
// skippable, so a task→agent pipeline would never exercise the skip at all —
// the test would pass while proving nothing.
func TestEndToEndTaskContextSurvivesACacheHit(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "runs.log")
	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: test
  plan:
  - task: run-tests
    inputs: []
    context: write
    run: |
      echo ran >> %[1]s
      mkdir -p context
      printf 'expired cert' > context/failure_cause
`, counter))

	mustRun(t, path)

	// The second run is where the claim lives: same content, so the task is a
	// cache hit and its command never runs again.
	mustRun(t, path)

	assertLineCount(t, counter, 1)

	byRun := storeContextByRun(t, path)
	if len(byRun) != 2 {
		t.Fatalf("run_context covers %d runs, want 2 — the skipped run must replay what it recorded, not skip silently (got %+v)", len(byRun), byRun)
	}

	for runID, facts := range byRun {
		if facts["failure_cause"] != "expired cert" {
			t.Errorf("run %s recorded %+v, want failure_cause=expired cert", runID, facts)
		}
	}
}

// TestEndToEndTaskContextReachesAnAgent proves the two halves meet: what a
// shell command wrote into context/ arrives in a later agent's recap, with no
// tool call and no file read on the agent's side.
func TestEndToEndTaskContextReachesAnAgent(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("Fixed."))
	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: read_file

jobs:
- name: test
  plan:
  - task: run-tests
    inputs: []
    context: write
    run: |
      mkdir -p context
      printf 'expired cert in the e2e suite' > context/failure_cause
  - agent: fixer
    prompt: Fix the cause.
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	results := fake.request(1).toolResults()
	if len(results) != 1 {
		t.Fatalf("agent's opening request carried %d tool results, want 1 (the recap); got %v", len(results), results)
	}

	if !strings.Contains(results[0], "expired cert in the e2e suite") {
		t.Errorf("recap = %q, want the fact the task wrote", results[0])
	}

	// Attribution names the task, so the agent can tell a machine-measured
	// fact from a model-authored one.
	if !strings.Contains(results[0], "run-tests") {
		t.Errorf("recap = %q, want the writing task named", results[0])
	}
}

// TestEndToEndBranchContextIsMergedByBranch is the claim that made context
// writes a load error inside a concurrent block until now: two branches
// recording the SAME key must not resolve to whichever finished last.
//
// Both branches write `finding`. They come back as two keys, named for the
// branch that recorded each, and the step after the block sees both.
func TestEndToEndBranchContextIsMergedByBranch(t *testing.T) {
	dir := t.TempDir()

	// Isolated on purpose: two concurrent TASKS record by writing files, and
	// under the shared strategy both would write one context/finding in one
	// build root and corrupt each other. That is a load error (see
	// rejectContextInBranches); this is the shape that works.
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: review
  plan:
  - in_parallel:
      steps:
      - task: security
        inputs: []
        outputs: []
        context: write
        run: |
          mkdir -p context
          printf 'hardcoded credential' > context/finding
      - task: perf
        inputs: []
        outputs: []
        context: write
        run: |
          mkdir -p context
          printf 'n+1 query' > context/finding
`)

	mustRun(t, path)

	facts := map[string]string{}
	for _, entry := range storeRunContext(t, path) {
		facts[entry.Key] = entry.Value
	}

	// Neither branch's fact was lost to the other's.
	want := map[string]string{
		"security.finding": "hardcoded credential",
		"perf.finding":     "n+1 query",
	}

	for key, value := range want {
		if facts[key] != value {
			t.Errorf("%s = %q, want %q (got all: %+v)", key, facts[key], value, facts)
		}
	}

	// And the unqualified key never appears: a bare `finding` would mean one
	// branch had silently overwritten the other.
	if _, collided := facts["finding"]; collided {
		t.Errorf("run context holds a bare %q; branch writes must be keyed by branch, got %+v", "finding", facts)
	}
}

// TestEndToEndNestedBranchContextSurvives is the nesting case, and it is a
// regression test: branch scopes were first derived from the RUN id, so a
// nested block inside two different branches computed the SAME scope for its
// own branch 0. The two tasks then overwrote each other's rows before either
// join saw them, and only one fact survived the whole run — the exact lost
// update branch scoping exists to prevent, one level down.
//
// Scopes now nest, and each join merges into its ENCLOSING scope rather than
// straight into the run, so the facts travel up one single-threaded step at a
// time.
func TestEndToEndNestedBranchContextSurvives(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: review
  plan:
  - in_parallel:
      steps:
      - in_parallel:
          steps:
          - task: scan
            inputs: []
            outputs: []
            context: write
            run: |
              mkdir -p context
              printf 'FROM-LEFT' > context/finding
      - in_parallel:
          steps:
          - task: scan
            inputs: []
            outputs: []
            context: write
            run: |
              mkdir -p context
              printf 'FROM-RIGHT' > context/finding
`)

	mustRun(t, path)

	facts := map[string]string{}
	for _, entry := range storeRunContext(t, path) {
		facts[entry.Key] = entry.Value
	}

	// Both reached the run scope, each still attributable. The nested blocks
	// have no step name of their own, so they are qualified by position —
	// without that they would collapse onto one key and the second would win.
	want := map[string]string{
		"branch0.scan.finding": "FROM-LEFT",
		"branch1.scan.finding": "FROM-RIGHT",
	}

	for key, value := range want {
		if facts[key] != value {
			t.Errorf("%s = %q, want %q (got all: %+v)", key, facts[key], value, facts)
		}
	}
}

// TestEndToEndRaceContextKeepsOnlyTheWinner proves a loser's partial facts are
// discarded with its workspace, the same treatment its execution log gets.
func TestEndToEndRaceContextKeepsOnlyTheWinner(t *testing.T) {
	dir := t.TempDir()
	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: pick
  plan:
  - race:
      steps:
      - task: quick
        inputs: []
        outputs: []
        context: write
        run: |
          mkdir -p context
          printf 'quick' > context/answer
      - task: slow
        inputs: []
        outputs: []
        context: write
        run: |
          sleep 5
          mkdir -p context
          printf 'slow' > context/answer
`)

	mustRun(t, path)

	facts := map[string]string{}
	for _, entry := range storeRunContext(t, path) {
		facts[entry.Key] = entry.Value
	}

	if facts["quick.answer"] != "quick" {
		t.Errorf("winner's fact = %q, want %q (got all: %+v)", facts["quick.answer"], "quick", facts)
	}

	if _, present := facts["slow.answer"]; present {
		t.Errorf("a cancelled racer's fact survived: %+v", facts)
	}
}

// storeContextByRun returns every recorded fact grouped by the run that
// recorded it — the shape that makes "did the cached run replay?" a direct
// question.
func storeContextByRun(t *testing.T, pipelinePath string) map[string]map[string]string {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(), `SELECT run_id, key, value FROM run_context WHERE run_id NOT LIKE '%#%' ORDER BY run_id, key`)
	if err != nil {
		t.Fatalf("query run_context: %v", err)
	}
	defer func() { _ = rows.Close() }()

	byRun := map[string]map[string]string{}

	for rows.Next() {
		var runID, key, value string

		err = rows.Scan(&runID, &key, &value)
		if err != nil {
			t.Fatalf("scan run_context: %v", err)
		}

		if byRun[runID] == nil {
			byRun[runID] = map[string]string{}
		}

		byRun[runID][key] = value
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("read run_context: %v", err)
	}

	return byRun
}

// contextRow is one run_context row, read back for assertions.
type contextRow struct {
	Key       string
	Value     string
	WrittenBy string
}

// storeRunContext returns the facts recorded at RUN scope — what a later step
// would actually be shown.
//
// Branch scopes are excluded (they carry a '#'): a concurrent branch records
// into a scope only it touches, which is then merged back under a
// branch-qualified key. Returning those raw would report every branch fact
// twice, once under its own bare key, which reads exactly like the collision
// this design exists to prevent.
func storeRunContext(t *testing.T, pipelinePath string) []contextRow {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(),
		`SELECT key, value, written_by FROM run_context WHERE run_id NOT LIKE '%#%' ORDER BY key`)
	if err != nil {
		t.Fatalf("query run_context: %v", err)
	}
	defer func() { _ = rows.Close() }()

	return scanContextRows(t, rows)
}

func scanContextRows(t *testing.T, rows *sql.Rows) []contextRow {
	t.Helper()

	var entries []contextRow

	for rows.Next() {
		var entry contextRow

		err := rows.Scan(&entry.Key, &entry.Value, &entry.WrittenBy)
		if err != nil {
			t.Fatalf("scan run_context: %v", err)
		}

		entries = append(entries, entry)
	}

	err := rows.Err()
	if err != nil {
		t.Fatalf("read run_context: %v", err)
	}

	return entries
}

// TestEndToEndContextVisibleInsideOneBranch covers the read side of branch
// scoping (#40.2): a step inside a concurrent branch sees what an EARLIER step
// of the same branch recorded.
//
// Writes go to a scope only the branch touches and reads used to come from the
// run alone, so those facts became visible only at the join — after the branch
// had finished, which is to say never, to anything inside it. Two sequential
// steps outside a block always saw each other, which is what kept the
// asymmetry hidden until a matrix was nested in a branch, exactly as here.
//
// The matrix's cells are serial, so the provider is reached in a fixed order
// and a positional script is honest: the second branch is a task and never
// talks to a model at all.
func TestEndToEndContextVisibleInsideOneBranch(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		// Cell 1 (record): writes the fact into the BRANCH's scope.
		callsTool("set_context", map[string]any{"key": "finding", "value": "expired cert"}),
		says("Recorded."),
		// Cell 2 (read): opens with a recap that must already carry it.
		says("Read it."),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: worker
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  system: You work.

jobs:
- name: audit
  plan:
  - in_parallel:
      steps:
      - across:
        - var: phase
          values: [record, read]
        agent: worker
        context: write
        prompt: "phase {{ .vars.phase }}"
      - task: unrelated
        inputs: []
        run: "true"
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// The reading cell's FIRST request. Without the layered read it carries no
	// recap at all: the fact is one scope below the run, and the run is empty
	// until the join.
	reading := fake.request(3)

	if !slices.Contains(reading.toolNames(), "read_context") {
		t.Fatalf("the reading cell was offered %v; without the branch's own facts there is no recap to re-read",
			reading.toolNames())
	}

	results := reading.toolResults()
	if len(results) != 1 {
		t.Fatalf("the reading cell opened with %d tool results, want the single synthetic recap: %v", len(results), results)
	}

	// Under the key it was WRITTEN with. The branch prefix is a merge-time
	// concern, so inside the branch a fact reads back by its own name — which
	// is what makes two steps in a branch behave like two steps outside one.
	if !strings.Contains(results[0], "finding: expired cert") {
		t.Errorf("recap = %q, want the fact this branch recorded, under its plain key", results[0])
	}

	// And it still arrives at the run, once, branch-qualified by the join.
	entries := storeRunContext(t, path)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Key, ".finding") {
		t.Errorf("run_context = %+v, want one branch-qualified finding", entries)
	}
}
