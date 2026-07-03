package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

// The examples double as the acceptance spec: these tests assert the exact
// traces documented in examples/*/README.md. Never assert LLM content —
// assert machine semantics.

// repoRoot is resolved from this file's location, immune to t.Chdir.
var repoRoot = func() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(file))
}()

func repoPath(t *testing.T, rel string) string {
	t.Helper()
	return filepath.Join(repoRoot, rel)
}

func newTestEngine(t *testing.T, mockScript string) (*engine.Engine, *journal.SQLiteStore) {
	t.Helper()
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), nil)
	script, err := provider.LoadScript(mockScript)
	if err != nil {
		t.Fatal(err)
	}
	eng.Mock = script
	return eng, store
}

func loadExample(t *testing.T, dir string) (*machine.Machine, string) {
	t.Helper()
	wf := repoPath(t, filepath.Join("examples", dir, "workflow.ts"))
	m, err := machine.Load(wf)
	if err != nil {
		t.Fatalf("load %s: %v", wf, err)
	}
	return m, repoPath(t, filepath.Join("examples", dir, "mock_responses.yaml"))
}

func article(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(repoPath(t, "examples/summarize-critic/fixtures/article.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// eventTrace filters journal events down to comparable tuples.
func eventTrace(t *testing.T, store journal.Store, runID string) (states []string, failedByClass map[string]int, transitions int) {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	failedByClass = map[string]int{}
	for _, ev := range events {
		switch ev.Type {
		case journal.StateEntered:
			s, _ := ev.Data["state"].(string)
			states = append(states, s)
		case journal.HandlerFailed:
			c, _ := ev.Data["class"].(string)
			failedByClass[c]++
		case journal.TransitionFired:
			transitions++
		}
	}
	return states, failedByClass, transitions
}

// TestSummarizeCriticMockTrace asserts the exact deterministic trace from
// examples/summarize-critic/README.md.
func TestSummarizeCriticMockTrace(t *testing.T) {
	t.Chdir(t.TempDir()) // file.write lands in an isolated cwd

	m, script := loadExample(t, "summarize-critic")
	eng, store := newTestEngine(t, script)

	res, err := eng.Start(context.Background(), m, map[string]any{"article": article(t)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if res.Status != journal.StatusDone || res.Terminal != "done" {
		t.Fatalf("status = %s at %s, want done at done", res.Status, res.Terminal)
	}
	if res.State.Transitions != 5 {
		t.Errorf("transitions = %d, want 5", res.State.Transitions)
	}
	if v := res.State.Visits["draft"]; v != 2 {
		t.Errorf("visits.draft = %d, want 2", v)
	}

	states, failed, _ := eventTrace(t, store, res.RunID)
	wantStates := []string{"draft", "critique", "draft", "critique", "publish"}
	if strings.Join(states, ",") != strings.Join(wantStates, ",") {
		t.Errorf("state sequence = %v, want %v", states, wantStates)
	}
	if failed["rate_limited"] != 1 {
		t.Errorf("rate_limited failures = %d, want 1 (transient retry)", failed["rate_limited"])
	}
	if failed["schema_violation"] != 1 {
		t.Errorf("schema_violation failures = %d, want 1 (semantic retry)", failed["schema_violation"])
	}

	// The artifact: out/summary.md with the three key points.
	summary, err := os.ReadFile("out/summary.md")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	for _, want := range []string{"Ideal X", "ISO standardization", "97 percent"} {
		if !strings.Contains(string(summary), want) {
			t.Errorf("summary.md missing %q", want)
		}
	}
}

// TestAdoptVariantTrace asserts the same routing as the sibling, plus the
// adopt-specific conversation shape: the article crosses into the drafter's
// conversation exactly once.
func TestAdoptVariantTrace(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExample(t, "summarize-critic-adopt")
	eng, store := newTestEngine(t, script)

	art := article(t)
	res, err := eng.Start(context.Background(), m, map[string]any{"article": art})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s, want done", res.Status)
	}
	if res.State.Transitions != 5 {
		t.Errorf("transitions = %d, want 5", res.State.Transitions)
	}

	states, _, _ := eventTrace(t, store, res.RunID)
	wantStates := []string{"draft", "critique", "draft", "critique", "publish"}
	if strings.Join(states, ",") != strings.Join(wantStates, ",") {
		t.Errorf("state sequence = %v, want %v", states, wantStates)
	}

	// Draft visit 2 adopted visit 1's conversation: its journaled messages
	// must contain visit 1's exchange (>= 4 messages) and the article marker
	// exactly once across the whole conversation.
	convo := res.State.Convos["draft"]
	if len(convo) < 4 {
		t.Fatalf("adopted draft conversation has %d messages, want >= 4 (replayed visit 1 + feedback + reply)", len(convo))
	}
	marker := "The Box That Shrank the World"
	count := 0
	for _, msg := range convo {
		count += strings.Count(msg.Text, marker)
	}
	if count != 1 {
		t.Errorf("article appears %d times in the drafter's conversation, want exactly 1 (never re-sent)", count)
	}
	// The feedback message must NOT contain the article (the {{ if }} branch).
	last := convo[len(convo)-2] // last user message before the final reply
	if strings.Contains(last.Text, marker) {
		t.Error("revision feedback message re-sent the article; adopt should not re-prime")
	}
}

// TestHumanGateParkAndResume: the critic never approves, the run parks at
// the gate, and resuming with an event routes and merges the gate's data.
func TestHumanGateParkAndResume(t *testing.T) {
	t.Chdir(t.TempDir())

	neverApproves := `
draft:
  - text: '{"summary": "Draft one.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft two.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft three.", "key_points": ["a", "b", "c"]}'
critique:
  - text: '{"score": 3, "issues": ["too short"], "event": "revise"}'
  - text: '{"score": 4, "issues": ["still short"], "event": "revise"}'
  - text: '{"score": 5, "issues": ["nope"], "event": "revise"}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(neverApproves), 0o644); err != nil {
		t.Fatal(err)
	}

	wf := repoPath(t, "examples/summarize-critic/workflow.ts")
	m, err := machine.Load(wf)
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, scriptPath)

	res, err := eng.Start(context.Background(), m, map[string]any{"article": article(t)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}
	if res.State.Current != "escalate" {
		t.Fatalf("parked at %s, want escalate", res.State.Current)
	}
	if v := res.State.Visits["draft"]; v != 3 {
		t.Errorf("visits.draft = %d, want 3 (guard-bounded revisions)", v)
	}

	// Resume in a "new process": fresh fold from the store.
	res2, err := eng.Resume(context.Background(), m, res.RunID, "approved", map[string]any{"note": "ship it"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != journal.StatusDone || res2.Terminal != "done" {
		t.Fatalf("resumed status = %s at %s, want done", res2.Status, res2.Terminal)
	}
	// The gate's data merged into ctx.
	gate, _ := res2.State.Ctx["escalate"].(map[string]any)
	if gate["note"] != "ship it" {
		t.Errorf("ctx.escalate = %v, want the resume data merged", gate)
	}
	// Resuming a finished run must fail.
	if _, err := eng.Resume(context.Background(), m, res.RunID, "approved", nil); err == nil {
		t.Error("resuming a finished run should error")
	}
	_ = store
}

// TestMaxTurnsOneSurvivesSemanticRetry: the turn budget bounds model calls
// within ONE conversation turn and resets per retry — max_turns: 1 on a
// tool-less state must not starve retry-with-feedback.
func TestMaxTurnsOneSurvivesSemanticRetry(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
export default {
  name: "tight",
  model: "mock",
  defaults: { maxTurns: 1 },
  states: {
    work: { prompt: "produce the thing", output: { answer: "string" } },
  },
};`
	script := `
work:
  - text: "not json at all"
  - text: '{"answer": "fixed"}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, scriptPath)
	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done — semantic retry must get a fresh turn budget", res.Status, res.Terminal)
	}
	out, _ := res.State.Ctx["work"].(map[string]any)
	if out["answer"] != "fixed" {
		t.Errorf("ctx.work = %v, want the corrected output", out)
	}
}

// TestPRReviewDeepPath asserts the token-funnel machine: per-file foreach
// scouting, filtered fan-out to the senior, and the guard vetoing the
// senior's "approve" while findings exist.
func TestPRReviewDeepPath(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExample(t, "pr-review")
	eng, store := newTestEngine(t, script)
	diff, err := os.ReadFile(repoPath(t, "examples/pr-review/fixtures/pr.diff"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{
		"diff":        string(diff),
		"root":        repoPath(t, "examples/pr-review/fixtures/repo"),
		"title":       "queue: parallel worker pool",
		"description": "Process jobs concurrently",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	// With a root, split_diff enriches each entry with the current file —
	// scouts see code around the patch, not just hunks.
	split, _ := res.State.Ctx["split_diff"].(map[string]any)
	files, _ := split["files"].([]any)
	if len(files) != 3 {
		t.Fatalf("split_diff.files = %d entries, want 3", len(files))
	}
	worker, _ := files[1].(map[string]any)
	content, _ := worker["content"].(string)
	if !strings.Contains(content, "func (p *Pool) Process") {
		t.Errorf("worker.go entry missing current-file context (got %d bytes)", len(content))
	}

	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{"split_diff", "scout_files", "scout_pr", "deep_review", "verdict", "write_review"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 6 {
		t.Errorf("transitions = %d, want 6", transitions)
	}

	scouts, _ := res.State.Ctx["scout_files"].(map[string]any)
	if n, _ := scouts["count"].(int); n != 3 {
		t.Errorf("scout_files.count = %v, want 3 (one hermetic context per file)", scouts["count"])
	}
	deep, _ := res.State.Ctx["deep_review"].(map[string]any)
	if n, _ := deep["count"].(int); n != 2 {
		t.Errorf("deep_review.count = %v, want 2 (low-risk file filtered out)", deep["count"])
	}

	// The senior proposed approve; the guard must have vetoed it: the fired
	// verdict transition is the fallback (on == "").
	verdict, _ := res.State.Ctx["verdict"].(map[string]any)
	if verdict["event"] != "approve" {
		t.Fatalf("verdict.event = %v, want approve (the proposal being vetoed)", verdict["event"])
	}
	events, err := store.Events(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != journal.TransitionFired {
			continue
		}
		if from, _ := ev.Data["from"].(string); from != "verdict" {
			continue
		}
		if on, _ := ev.Data["on"].(string); on != "" {
			t.Errorf("verdict transition fired on %q, want fallback (guard-vetoed approve)", on)
		}
	}

	review, err := os.ReadFile("out/review.md")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	for _, wantStr := range []string{"concurrent map write", "wg.Add", "store.Find"} {
		if !strings.Contains(string(review), wantStr) {
			t.Errorf("review.md missing %q", wantStr)
		}
	}
}

// TestPRReviewTrivialPath: every scout reports low risk, the guard allows the
// trivial skip, and the senior model never runs.
func TestPRReviewTrivialPath(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := repoPath(t, "examples/pr-review/workflow.ts")
	m, err := machine.Load(wf)
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, repoPath(t, "examples/pr-review/mock_trivial.yaml"))
	diff, err := os.ReadFile(repoPath(t, "examples/pr-review/fixtures/pr.diff"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{"diff": string(diff)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s, want done", res.Status)
	}

	states, _, _ := eventTrace(t, store, res.RunID)
	want := []string{"split_diff", "scout_files", "scout_pr", "note_trivial"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v — the senior must never run", states, want)
	}
	review, err := os.ReadFile("out/review.md")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if !strings.Contains(string(review), "no senior review needed") {
		t.Errorf("review.md = %q, want the triage note", string(review))
	}
}

// TestMemoReplaysAcrossRuns: run the same machine twice against one store —
// the second run's memo states replay cached outputs, spend zero tokens, and
// never touch the model.
func TestMemoReplaysAcrossRuns(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExample(t, "pr-review")
	eng, store := newTestEngine(t, script)
	diff, err := os.ReadFile(repoPath(t, "examples/pr-review/fixtures/pr.diff"))
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"diff": string(diff), "title": "queue: parallel worker pool", "description": "Process jobs concurrently"}

	first, err := eng.Start(context.Background(), m, input)
	if err != nil || first.Status != journal.StatusDone {
		t.Fatalf("first run: %v (%v)", err, first)
	}

	// Second run: mock queues are per-run (fresh), but memo hits mean the
	// scouts and senior never consume them.
	second, err := eng.Start(context.Background(), m, input)
	if err != nil || second.Status != journal.StatusDone {
		t.Fatalf("second run: %v (%v)", err, second)
	}
	if got := second.State.Usage.Total(); got != 0 {
		t.Errorf("second run spent %d tokens, want 0 (every agent state memoized)", got)
	}
	scouts, _ := second.State.Ctx["scout_files"].(map[string]any)
	if scouts["memo_hits"] != 3 {
		t.Errorf("scout_files.memo_hits = %v, want 3 (unchanged files are free on re-review)", scouts["memo_hits"])
	}
	deep, _ := second.State.Ctx["deep_review"].(map[string]any)
	if deep["memo_hits"] != 2 {
		t.Errorf("deep_review.memo_hits = %v, want 2", deep["memo_hits"])
	}
	_ = store
}

// TestForEachSkipOnFailure: one poisoned item is skipped, the rest survive,
// and the aggregate reports it for guards.
func TestForEachSkipOnFailure(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const seed = {
  action: "diff.split",
  input: {
    diff: [
      "diff --git a/one.go b/one.go", "+a",
      "diff --git a/two.go b/two.go", "+b",
      "diff --git a/three.go b/three.go", "+c",
    ].join("\n"),
  },
};
const work = {
  forEach: { over: ({ seed }) => seed.files, as: "file", onItemFailure: "skip" },
  retry: "none",
  prompt: ({ file }) => "look at " + file.path,
  output: { path: "string" },
};
export default {
  name: "skips",
  model: "mock",
  states: { seed, work },
  flow: pipe(seed, branch(work, [
    when(({ output }) => output.skipped === 1 && output.count === 2).to(done),
    fail,
  ])),
};`
	script := `
work:
  - text: '{"path": "one.go"}'
  - error: provider_error
  - text: '{"path": "three.go"}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, scriptPath)
	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done via the skipped==1 guard", res.Status, res.Terminal)
	}
	work, _ := res.State.Ctx["work"].(map[string]any)
	if work["skipped"] != 1 || work["count"] != 2 {
		t.Errorf("work aggregate = %v, want count 2 / skipped 1", work)
	}
}

// TestMemoReplayKeepsEvent: a memoized event-routing state must route by the
// CACHED event on replay — not silently take the fallback.
func TestMemoReplayKeepsEvent(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const decide = { memo: true, prompt: "pick", output: { x: "string" }, events: ["yes", "no"] };
const won = { write: "out/w.txt", content: "won" };
export default {
  name: "memoevent",
  model: "mock",
  states: { decide, won },
  flow: pipe(branch(decide, { yes: won, else: fail })),
};`
	script := `
decide:
  - text: '{"x": "a", "event": "yes"}'
  - text: '{"x": "a", "event": "yes"}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, scriptPath)

	first, err := eng.Start(context.Background(), m, nil)
	if err != nil || first.Status != journal.StatusDone {
		t.Fatalf("first run: %v (%v)", err, first)
	}
	second, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if second.Status != journal.StatusDone {
		t.Fatalf("memo replay lost the event: status = %s at %s, want done via yes", second.Status, second.Terminal)
	}
	if second.State.Usage.Total() != 0 {
		t.Errorf("second run spent %d tokens, want 0 (memo hit)", second.State.Usage.Total())
	}
}

// TestVisitsDefinedForUnvisitedStates: `visits.x < N` on a never-entered
// state must read 0, not undefined (undefined < N is false in JS).
func TestVisitsDefinedForUnvisitedStates(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const a = { prompt: "go", output: { x: "string" } };
const b = { prompt: "go2" };
export default {
  name: "visits0",
  model: "mock",
  states: { a, b },
  flow: pipe(branch(a, [
    when(({ visits }) => visits.b < 1).to(b),
    fail,
  ])),
};`
	script := `
a:
  - text: '{"x": "1"}'
b:
  - text: "hello"
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, scriptPath)
	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done — visits.b should read 0, not undefined", res.Status, res.Terminal)
	}
}

// TestResumeWithoutEventFails: parked gates demand an explicit event.
func TestResumeWithoutEventFails(t *testing.T) {
	t.Chdir(t.TempDir())

	script := `
draft:
  - text: '{"summary": "One.", "key_points": ["a","b","c"]}'
  - text: '{"summary": "Two.", "key_points": ["a","b","c"]}'
  - text: '{"summary": "Three.", "key_points": ["a","b","c"]}'
critique:
  - text: '{"score": 1, "issues": ["x"], "event": "revise"}'
  - text: '{"score": 1, "issues": ["x"], "event": "revise"}'
  - text: '{"score": 1, "issues": ["x"], "event": "revise"}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	if err := os.WriteFile(scriptPath, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := machine.Load(repoPath(t, "examples/summarize-critic/workflow.ts"))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, scriptPath)
	res, err := eng.Start(context.Background(), m, map[string]any{"article": "short article"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}
	if _, err := eng.Resume(context.Background(), m, res.RunID, "", nil); err == nil {
		t.Error("resume without an event should error while parked")
	}
}

// TestCodegenMockTrace asserts the exact deterministic trace from
// examples/codegen/README.md: the reader loop rejects once, the coder
// regenerates, and gate two ACTUALLY RUNS the generated test — the second
// gate is real, not scripted. exec.run returns a non-zero exit as data, so
// here (exit 0) the machine ships; a red build would loop instead.
func TestCodegenMockTrace(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExample(t, "codegen")
	eng, store := newTestEngine(t, script)
	spec, err := os.ReadFile(repoPath(t, "examples/codegen/fixtures/spec.md"))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Start(context.Background(), m, map[string]any{
		"spec":       string(spec),
		"language":   "bash",
		"out":        "out",
		"verify_cmd": "bash greet_test.sh", // the real ground-truth gate
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	// Reader rejected once, so generate/review each ran twice before disk.
	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{"plan", "generate", "review", "generate", "review", "write_files", "build", "report"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 8 {
		t.Errorf("transitions = %d, want 8", transitions)
	}

	// Gate two really executed the generated test: its exit code is DATA.
	build, _ := res.State.Ctx["build"].(map[string]any)
	if build["ok"] != true || build["exit_code"] != 0 {
		t.Fatalf("build gate = %v, want ok:true exit:0 (test actually ran)", build)
	}
	if out, _ := build["stdout"].(string); !strings.Contains(out, "all tests passed") {
		t.Errorf("build stdout = %q, want the generated test's own output", build["stdout"])
	}

	gen, _ := res.State.Ctx["generate"].(map[string]any)
	if n, _ := gen["count"].(int); n != 2 {
		t.Errorf("generate.count = %v, want 2 (one hermetic context per planned file)", gen["count"])
	}

	// The approved code reached disk and runs for real.
	greet, err := os.ReadFile("out/greet.sh")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	if !strings.Contains(string(greet), "shout") {
		t.Errorf("out/greet.sh missing the revised --shout handling")
	}
	manifest, err := os.ReadFile("out/GENERATED.md")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !strings.Contains(string(manifest), "PASSED") {
		t.Errorf("GENERATED.md should record the build gate as PASSED")
	}
}
