package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

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
func eventFixture(t *testing.T) (*config.Config, *config.Job, *store.Store, workspace.Provider) {
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

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"))
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

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	defer func() { _ = st.Close() }()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	job, err := cfg.FindJob("build")
	if err != nil {
		t.Fatal(err)
	}

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

	return collected
}
