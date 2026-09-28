package pipeline

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/merkle"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestRunJobLogsCarryTheRunID proves the run-level bracket (job.run/job.done)
// and every step's completion (job.step.finished) reach slog with the SAME
// run id — an operator reading --log-level output, not just the event bus,
// otherwise had no way to correlate a step's completion to the run/job it
// belonged to, or even to know a step had finished at all (see job.step's
// own pre-execution Debug line, which had no completion counterpart).
func TestRunJobLogsCarryTheRunID(t *testing.T) {
	// Not t.Parallel(): mutates slog's default logger.
	cfg, job, st, provider := eventFixture(t)
	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(events.LogHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))))
	t.Cleanup(func() { slog.SetDefault(prev) })

	err := RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	out := buf.String()

	runLine := findLogLine(t, out, "job.run")
	runID := logField(runLine, "run")

	if runID == "" {
		t.Fatalf("job.run carried no run id: %s", runLine)
	}

	for _, msg := range []string{"job.done", "job.step.finished"} {
		line := findLogLine(t, out, msg)
		if got := logField(line, "run"); got != runID {
			t.Errorf("%s run=%q, want %q (job.run's id): %s", msg, got, runID, line)
		}
	}

	if strings.Count(out, "job.step.finished") != len(job.Plan) {
		t.Errorf("expected one job.step.finished per plan step (%d), got %d in: %s", len(job.Plan), strings.Count(out, "job.step.finished"), out)
	}
}

// findLogLine returns the first log line whose message is msg, failing the
// test if none does. The message is matched as a whole field so a value that
// happens to contain it cannot stand in for the line itself.
func findLogLine(t *testing.T, out, msg string) string {
	t.Helper()

	for _, line := range strings.Split(out, "\n") {
		for _, field := range strings.Fields(line) {
			if field == msg || field == "msg="+msg {
				return line
			}
		}
	}

	t.Fatalf("no log line found for %q in: %s", msg, out)

	return ""
}

// logField extracts a key=value field from a log line, or "" when absent.
func logField(line, key string) string {
	for _, field := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(field, key+"="); ok {
			return v
		}
	}

	return ""
}

// TestRunJobPublishesRunEvents is the whole live/post-hoc contract in one
// pass: a real job run publishes a bracketed, ordered event stream, and the
// second run of the same unchanged pipeline reports its steps as SKIPPED
// rather than run — which is what the UI renders folded.
func TestRunJobPublishesRunEvents(t *testing.T) {
	t.Parallel()

	cfg, job, st, provider := eventFixture(t)
	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var collected []events.Event

	bus := events.New(func(e events.Event) { collected = append(collected, e) })
	ctx := events.WithBus(context.Background(), bus)

	err := RunJob(ctx, cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	bus.Close()

	runID := assertBracketedStream(t, collected)

	// The run is readable back from the store, which is what the web UI does.
	row, ok, err := st.FindRunRow(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("FindRunRow: ok=%v err=%v", ok, err)
	}

	if row.Status != "succeeded" || row.FinishedAt.IsZero() {
		t.Errorf("stored run = %+v, want succeeded with a finish time", row)
	}

	// Second run: identical content, so the chain is cached and the steps are
	// reported skipped — the distinction the transcript is built to show.
	collected = nil
	bus2 := events.New(func(e events.Event) { collected = append(collected, e) })

	err = RunJob(events.WithBus(context.Background(), bus2), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("second RunJob: %v", err)
	}

	bus2.Close()

	skipped := countSkips(t, collected)

	if skipped == 0 {
		t.Error("the second run of an unchanged pipeline published no skip events")
	}
}

// assertBracketedStream checks the shape every run's event stream must have:
// bracketed by job_started/job_finished, one started and one finished event
// per step, and a run id on every event. It returns the run id.
func assertBracketedStream(t *testing.T, collected []events.Event) string {
	t.Helper()

	var (
		started, finished int
		runID             string
	)

	for _, event := range collected {
		if event.RunID == "" {
			t.Errorf("event %q carries no run id", event.Type)
		}

		runID = event.RunID

		switch event.Type {
		case events.TypeStepStarted:
			started++
		case events.TypeStepFinished:
			finished++
		}
	}

	if started != 2 || finished != 2 {
		t.Errorf("got %d started / %d finished step events, want 2 / 2", started, finished)
	}

	if collected[0].Type != events.TypeJobStarted {
		t.Errorf("first event = %q, want job_started", collected[0].Type)
	}

	if last := collected[len(collected)-1]; last.Type != events.TypeJobFinished || last.Status != "succeeded" {
		t.Errorf("last event = %q/%q, want job_finished/succeeded", last.Type, last.Status)
	}

	return runID
}

// countSkips counts skip events, insisting each one says WHY — a skip with no
// reason is the event that makes a transcript useless.
func countSkips(t *testing.T, collected []events.Event) int {
	t.Helper()

	skipped := 0

	for _, event := range collected {
		if event.Type != events.TypeStepSkipped {
			continue
		}

		skipped++

		if event.Text == "" {
			t.Error("a skipped step published no reason")
		}
	}

	return skipped
}

// eventFixture builds a two-task pipeline with its store and workspace
// provider — everything RunJob needs and nothing the assertions care about.
func eventFixture(t *testing.T) (*config.Config, *config.Job, store.Store, workspace.Provider) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.yml")

	err := os.WriteFile(path, []byte(`
jobs:
  - name: build
    plan:
      - task: compile
        run: "true"
      - task: verify
        run: "true"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	job, err := cfg.FindJob("build")
	if err != nil {
		t.Fatal(err)
	}

	return cfg, job, st, provider
}

// TestRunJobPublishesTaskOutput covers the gap a transcript had until now: a
// step that SUCCEEDED showed nothing, so "what did it print" had no answer
// short of scrolling back through the terminal that ran it.
func TestRunJobPublishesTaskOutput(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: speak
        run: echo hello from the task
      - task: quiet
        run: "true"
`, false)

	outputs := map[string]string{}

	for _, event := range collected {
		if event.Type == events.TypeStepOutput {
			outputs[event.StepName] = event.Text
		}
	}

	if got := outputs["speak"]; got != "hello from the task" {
		t.Errorf("speak output = %q, want %q", got, "hello from the task")
	}

	// A step that printed nothing publishes nothing: an empty log block is
	// worse than no log block.
	if _, ok := outputs["quiet"]; ok {
		t.Errorf("a silent task published an output event: %q", outputs["quiet"])
	}
}

// TestFailedTaskPublishesItsOutput is the case the feature exists for: the
// step someone opens a run page to investigate. Nothing else carries a
// failing command's output — the error names the exit status and no more —
// so suppressing it here would leave the transcript answering "what did it
// print" only for the steps nobody needs to ask about.
func TestFailedTaskPublishesItsOutput(t *testing.T) {
	t.Parallel()

	// The command GENERATES its output rather than echoing a literal, so the
	// second assertion below cannot be satisfied by the command text itself
	// appearing in the error.
	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: boom
        run: seq 3; exit 2
`, true)

	var published string

	for _, event := range collected {
		if event.Type == events.TypeStepOutput {
			published = event.Text
		}
	}

	if published != "1\n2\n3" {
		t.Errorf("failed task output = %q, want %q", published, "1\n2\n3")
	}

	// The error is genuinely separate text, so showing both is not the same
	// thing twice — which is what the suppression this replaced assumed.
	for _, event := range collected {
		if event.Type == events.TypeStepFinished && strings.Contains(event.Text, published) {
			t.Errorf("the error already carried the output, so suppressing it would have been right: %q", event.Text)
		}
	}
}

// runFixturePipeline runs a one-off pipeline and returns everything it
// published. wantFailure says which outcome the fixture is written to produce,
// so a fixture that stops failing (or starts) is caught rather than silently
// changing what the assertions see.
func runFixturePipeline(t *testing.T, yaml string, wantFailure bool) []events.Event {
	t.Helper()

	collected, _ := runFixtureBeside(t, yaml, wantFailure, nil)

	return collected
}

// runFixtureBeside is runFixturePipeline handing back the store, still open,
// for assertions about what the run recorded. beside, when set, runs
// concurrently with the job — a person answering an approval — and is waited
// for before this returns.
func runFixtureBeside(t *testing.T, yaml string, wantFailure bool, beside func(store.Store)) ([]events.Event, store.Store) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	job, err := cfg.FindJob("build")
	if err != nil {
		t.Fatal(err)
	}

	defer runBeside(st, beside)()

	var collected []events.Event

	bus := events.New(func(e events.Event) { collected = append(collected, e) })

	runErr := RunJob(events.WithBus(context.Background(), bus), cfg, job, nil, provider, st, false)

	bus.Close()

	if wantFailure && runErr == nil {
		t.Fatal("expected the fixture pipeline to fail")
	}

	if !wantFailure && runErr != nil {
		t.Fatalf("RunJob: %v", runErr)
	}

	return collected, st
}

// TestGuardSkippedStepClosesItsOwnStart pins the identity a skip event must
// carry: a when:-guarded step already published step_started under a minted
// id, so its skip has to report THAT id rather than mint a second one.
//
// A fresh mark made the skip a CHILD of the start (the context inside the
// step already names the start as the container), so the web UI's tree held
// an entry that never closed — a finished job rendered a step whose elapsed
// timer kept counting.
func TestGuardSkippedStepClosesItsOwnStart(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
  - name: build
    plan:
      - task: ran
        run: "true"
      - task: guarded
        when: "false"
        run: "true"
`, false)

	var started, skipped *events.Event

	for i, event := range collected {
		if event.StepName != "guarded" {
			continue
		}

		switch event.Type {
		case events.TypeStepStarted:
			started = &collected[i]
		case events.TypeStepSkipped:
			skipped = &collected[i]
		}
	}

	if started == nil || skipped == nil {
		t.Fatalf("guarded step published started=%v skipped=%v, want both", started, skipped)
	}

	if skipped.StepID != started.StepID {
		t.Errorf("skip step id = %d, want %d (the id its start published)", skipped.StepID, started.StepID)
	}

	if skipped.ParentStepID != started.ParentStepID {
		t.Errorf("skip parent = %d, want %d — a skip is not nested inside its own start", skipped.ParentStepID, started.ParentStepID)
	}
}

// TestFailedStepPublishesTheNodeItFailedUnder: the web page marks a step
// "changed" by comparing the hash its step_finished carried with the last
// passed run's, so a failure that published none could never be marked —
// on exactly the step that broke. Each kind that records a node before it
// fails must publish that node.
func TestFailedStepPublishesTheNodeItFailedUnder(t *testing.T) {
	t.Parallel()

	resources := `
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
    in: "true"
    out: "exit 1"
- name: broken
  config:
    check: printf '[{"n":"1"}]'
    in: "exit 1"
resources:
- name: ticks
  type: counter
  source: {}
- name: flaky
  type: broken
  source: {}
`

	cases := []struct {
		name   string
		kind   string
		step   string
		yaml   string
		beside func(store.Store)
	}{
		{name: "task", kind: "task", step: "boom", yaml: `
jobs:
- name: build
  plan:
  - task: boom
    run: exit 1
`},
		{name: "put", kind: "put", step: "ticks", yaml: resources + `
jobs:
- name: build
  plan:
  - get: ticks
  - put: ticks
    inputs: []
`},
		{name: "in-place get", kind: "get", step: "flaky", yaml: resources + `
jobs:
- name: build
  plan:
  - get: ticks
  - get: flaky
`},
		{name: "rejected approval", kind: "approval", step: "", yaml: `
jobs:
- name: build
  plan:
  - approval:
      message: ship it?
`, beside: rejectFirstApproval(t)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			collected, st := runFixtureBeside(t, tc.yaml, true, tc.beside)

			finished := findStepEvent(collected, events.TypeStepFinished, tc.kind, tc.step)
			if finished == nil {
				t.Fatalf("%s %s published no step_finished", tc.kind, tc.step)
			}

			if finished.Hash == "" {
				t.Fatalf("failed %s %s published no hash", tc.kind, tc.step)
			}

			nodes, err := st.NodesByHash(context.Background(), []string{finished.Hash})
			if err != nil {
				t.Fatal(err)
			}

			node, ok := nodes[finished.Hash]
			if !ok {
				t.Fatalf("published hash %s names no recorded node", finished.Hash)
			}

			if node.Status == "succeeded" {
				t.Errorf("the published node is %s, want the failure it recorded", node.Status)
			}
		})
	}
}

// runBeside starts beside against st, if there is one, and returns what waits
// for it to finish.
func runBeside(st store.Store, beside func(store.Store)) func() {
	if beside == nil {
		return func() {}
	}

	done := make(chan struct{})

	go func() {
		defer close(done)
		beside(st)
	}()

	return func() { <-done }
}

// rejectFirstApproval says no to the first approval the run asks for, as a
// person at `steps approvals reject` would.
func rejectFirstApproval(t *testing.T) func(store.Store) {
	t.Helper()

	return func(st store.Store) {
		deadline := time.Now().Add(10 * time.Second)

		for time.Now().Before(deadline) {
			pending, err := st.Approvals(context.Background(), true, 1)
			if err == nil && len(pending) > 0 {
				_ = st.DecideApproval(context.Background(), pending[0].ID, "rejected", "tester", "no")

				return
			}

			time.Sleep(10 * time.Millisecond)
		}
	}
}

func findStepEvent(collected []events.Event, eventType, stepKind, name string) *events.Event {
	for i := range collected {
		if collected[i].Type == eventType && collected[i].StepKind == stepKind && collected[i].StepName == name {
			return &collected[i]
		}
	}

	return nil
}

// TestRoutedFailureChainsUnderItsParent is why a failure's hash is published
// from its own field: a failure a to: route consumes carries the walk on, and
// the step it routes to must hash under the parent the failed step ran
// beneath — chaining under the failure would move every later cache key. A
// block is the same: its row names the node it recorded as failed, and
// nothing chains under it.
func TestRoutedFailureChainsUnderItsParent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, kind, failed, yaml string
	}{
		{name: "task", kind: "task", failed: "boom", yaml: `
  - task: boom
    run: exit 1
    to: {failure: recover}
`},
		{name: "do block", kind: "do", yaml: `
  - do:
    - task: boom
      run: exit 1
    to: {failure: recover}
`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			collected, st := runFixtureBeside(t, `
jobs:
- name: build
  plan:
  - task: first
    run: "true"
`+tc.yaml+`  - task: recover
    run: "true"
`, false, nil)

			first := findStepEvent(collected, events.TypeStepFinished, "task", "first")
			failed := findKindEvent(collected, events.TypeStepFinished, tc.kind, tc.failed)
			recovered := findStepEvent(collected, events.TypeStepFinished, "task", "recover")

			if first == nil || failed == nil || recovered == nil {
				t.Fatalf("missing step_finished: first=%v failed=%v recover=%v", first, failed, recovered)
			}

			if failed.Hash == "" {
				t.Fatal("the routed failure published no hash; this test guards nothing")
			}

			nodes, err := st.NodesByHash(context.Background(), []string{recovered.Hash, failed.Hash})
			if err != nil {
				t.Fatal(err)
			}

			if got := nodes[recovered.Hash].ParentHash; got != first.Hash {
				t.Errorf("recover chained under %q, want %q (the step before the failure)", got, first.Hash)
			}

			if node, ok := nodes[failed.Hash]; !ok || node.Status != "failed" {
				t.Errorf("the failure's row names %+v (recorded=%v), want the node it recorded as failed", node, ok)
			}
		})
	}
}

// findKindEvent is findStepEvent for a step that may publish under a name the
// fixture does not spell: an empty name matches the first of its kind.
func findKindEvent(collected []events.Event, eventType, stepKind, name string) *events.Event {
	if name != "" {
		return findStepEvent(collected, eventType, stepKind, name)
	}

	for i := range collected {
		if collected[i].Type == eventType && collected[i].StepKind == stepKind {
			return &collected[i]
		}
	}

	return nil
}

// TestGuardSkipPublishesNoHash: a skip row is compared against the last
// passed run like any other, so it must carry its OWN node — a parent's hash
// would mark a step changed whenever its neighbour differed — and a when:
// skip was never hashed, so it has none.
func TestGuardSkipPublishesNoHash(t *testing.T) {
	t.Parallel()

	collected := runFixturePipeline(t, `
jobs:
- name: build
  plan:
  - task: ran
    run: "true"
  - task: guarded
    when: "false"
    run: "true"
`, false)

	skipped := findStepEvent(collected, events.TypeStepSkipped, "task", "guarded")
	if skipped == nil {
		t.Fatal("guarded published no skip")
	}

	if skipped.Hash != "" {
		t.Errorf("a when: skip published %q, want no hash", skipped.Hash)
	}
}

// TestChainSkipPublishesTheNodeItMatched: the walk carries on under the
// parent after a chain skip, while the row names the node the cache matched.
func TestChainSkipPublishesTheNodeItMatched(t *testing.T) {
	t.Parallel()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { workspace.CloseBuild(bw, "test") })

	step := config.Step{Task: "compile"}
	cfg := &config.Config{Tasks: []config.Task{{Name: "compile", Run: "true"}}}
	parent := "parent-hash"

	own := taskHash(t, cfg, step, parent)

	var collected []events.Event

	bus := events.New(func(e events.Event) { collected = append(collected, e) })
	ctx := events.WithBus(context.Background(), bus)

	res, err := runNonGetStep(ctx, stepRunner{cfg: cfg, jobName: "build", bw: bw}, 0, step, map[string]bool{own: true}, parent)

	bus.Close()

	if err != nil || res.disposition != stepChainSkipped {
		t.Fatalf("res=%+v err=%v, want a chain skip", res, err)
	}

	if res.hash != parent {
		t.Errorf("the walk chains under %q after a chain skip, want the parent %q", res.hash, parent)
	}

	skipped := findStepEvent(collected, events.TypeStepSkipped, "task", "compile")
	if skipped == nil || skipped.Hash != own {
		t.Errorf("the chain skip published %+v, want its own hash %q", skipped, own)
	}
}

// taskHash is the node hash runTaskStep gives step under parent.
func taskHash(t *testing.T, cfg *config.Config, step config.Step, parent string) string {
	t.Helper()

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	content, err := merkle.TaskNodeContent(cfg, step, rt)
	if err != nil {
		t.Fatal(err)
	}

	hash, err := merkle.HashNode(merkle.NodeKindTask, content, parent)
	if err != nil {
		t.Fatal(err)
	}

	return hash
}

// TestChainReplayPublishesEachStepsOwnNode: a failed run is compared against
// the last passed one, and running an unchanged job twice makes that a replay
// whose steps past the skip point are published without running. Each must
// carry the node it replayed — not the skip point's, not "" — or the step that
// later breaks has nothing to be compared with.
func TestChainReplayPublishesEachStepsOwnNode(t *testing.T) {
	t.Parallel()

	run := fixtureRunner(t, `
jobs:
- name: build
  plan:
  - task: prep
    run: "true"
  - task: compile
    run: "true"
  - task: package
    run: "true"
`)

	ran, replayed := run(), run()

	for _, name := range []string{"prep", "compile", "package"} {
		finished := findStepEvent(ran, events.TypeStepFinished, "task", name)
		skipped := findStepEvent(replayed, events.TypeStepSkipped, "task", name)

		if finished == nil || skipped == nil {
			t.Fatalf("%s: finished=%v skipped=%v, want a run then a replay", name, finished, skipped)
		}

		if finished.Hash == "" || skipped.Hash != finished.Hash {
			t.Errorf("%s replayed as %q, want the node it ran as %q", name, skipped.Hash, finished.Hash)
		}
	}
}

// TestChainReplayAtTheFirstGetPublishesTheRest: a plan that opens with a get
// is skipped at that get, on the fan-out path — the commonest fully-cached
// run. Publishing only the get left every later step absent, so a failed run
// compared against it marked each one "new".
func TestChainReplayAtTheFirstGetPublishesTheRest(t *testing.T) {
	t.Parallel()

	run := fixtureRunner(t, `
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
    in: "true"
resources:
- name: ticks
  type: counter
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
  - task: compile
    run: "true"
`)

	ran, replayed := run(), run()

	finished := findStepEvent(ran, events.TypeStepFinished, "task", "compile")
	skipped := findStepEvent(replayed, events.TypeStepSkipped, "task", "compile")

	if finished == nil || skipped == nil {
		t.Fatalf("compile: finished=%v skipped=%v, want a run then a replay", finished, skipped)
	}

	if finished.Hash == "" || skipped.Hash != finished.Hash {
		t.Errorf("compile replayed as %q, want the node it ran as %q", skipped.Hash, finished.Hash)
	}

	get := findStepEvent(replayed, events.TypeStepSkipped, "get", "ticks")
	if get == nil || skipped.ParentStepID != get.StepID {
		t.Errorf("compile replayed outside the get that skipped it: get=%v compile parent=%d", get, skipped.ParentStepID)
	}
}

// fixtureRunner loads yaml's build job over one store and returns what runs
// it — green, or the test fails — handing back everything that run published.
func fixtureRunner(t *testing.T, yaml string) func() []events.Event {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	job, err := cfg.FindJob("build")
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	return func() []events.Event {
		var collected []events.Event

		bus := events.New(func(e events.Event) { collected = append(collected, e) })

		runErr := RunJob(events.WithBus(context.Background(), bus), cfg, job, nil, provider, st, false)

		bus.Close()

		if runErr != nil {
			t.Fatalf("RunJob: %v", runErr)
		}

		return collected
	}
}

type refusingEvents struct{ store.Events }

func (refusingEvents) AppendRunEvent(context.Context, store.RunEventRow) error {
	return os.ErrPermission
}

// TestAnUnpersistedEventNamesItsRun: the sink runs off the run's context, so
// the one line saying a transcript lost an event carries the run itself — a
// daemon's several pipelines share the log.
func TestAnUnpersistedEventNamesItsRun(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	StoreSink(refusingEvents{})(events.Event{RunID: "r-42", Type: events.TypeStepFinished})

	if !strings.Contains(buf.String(), "run=r-42") {
		t.Fatalf("line = %q", buf.String())
	}
}
