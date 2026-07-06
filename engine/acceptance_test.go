package engine_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/internal/ghfake"
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
		//exhaustive:ignore // the trace this test asserts on only tracks these three signals
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
	err := os.WriteFile(scriptPath, []byte(neverApproves), 0o600)
	if err != nil {
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
	checkParkedAtEscalate(t, res, 3)

	// The park journals the gate's declared choices — the answer surface any
	// later CLI resume or webview renders from the journal alone.
	checkEscalateChoices(t, res.State.Parked.Choices)

	// An event outside the gate's alphabet fails cleanly, before any routing.
	_, err = eng.Resume(context.Background(), m, res.RunID, "shipit", nil)
	checkOutOfAlphabetRejected(t, err)

	// Resume in a "new process": fresh fold from the store.
	res2, err := eng.Resume(context.Background(), m, res.RunID, "approved", map[string]any{"note": "ship it"})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	checkResumedDoneWithGateData(t, res2)

	// Resuming a finished run must fail.
	_, err = eng.Resume(context.Background(), m, res.RunID, "approved", nil)
	if err == nil {
		t.Error("resuming a finished run should error")
	}
	_ = store
}

func checkParkedAtEscalate(t *testing.T, res *engine.Result, wantVisits int) {
	t.Helper()
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}
	if res.State.Current != "escalate" {
		t.Fatalf("parked at %s, want escalate", res.State.Current)
	}
	if v := res.State.Visits["draft"]; v != wantVisits {
		t.Errorf("visits.draft = %d, want %d (guard-bounded revisions)", v, wantVisits)
	}
}

func checkEscalateChoices(t *testing.T, c *journal.ParkChoices) {
	t.Helper()
	if c == nil || c.Kind != "single" || len(c.Options) != 2 {
		t.Fatalf("parked choices = %+v, want single with the 2 declared options", c)
	}
	if c.Options[0].Event != "approved" || c.Options[0].Label != "Ship the current draft as-is" ||
		c.Options[1].Event != "rejected" || c.Options[1].Label != "Fail the run" {
		t.Errorf("options = %+v, want the workflow.ts choices in declaration order", c.Options)
	}
}

func checkOutOfAlphabetRejected(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), `no route for event "shipit"`) {
		t.Errorf("out-of-alphabet resume err = %v, want a closed-alphabet rejection", err)
	}
}

func checkResumedDoneWithGateData(t *testing.T, res *engine.Result) {
	t.Helper()
	if res.Status != journal.StatusDone || res.Terminal != "done" {
		t.Fatalf("resumed status = %s at %s, want done", res.Status, res.Terminal)
	}
	// The gate's data merged into ctx.
	gate, _ := res.State.Ctx["escalate"].(map[string]any)
	if gate["note"] != "ship it" {
		t.Errorf("ctx.escalate = %v, want the resume data merged", gate)
	}
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
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	checkSplitDiffHasFileContext(t, res)

	states, _, transitions := eventTrace(t, store, res.RunID)
	// fetch_pr passes the supplied diff straight through (no gh); with no `pr`
	// the review ends at write_review and never reaches the live publish tail.
	want := []string{"fetch_pr", "split_diff", "scout_files", "scout_pr", "deep_review", "verdict", "write_review"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 7 {
		t.Errorf("transitions = %d, want 7", transitions)
	}

	checkScoutAndDeepReviewCounts(t, res)

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
	checkVerdictTransitionVetoed(t, events)

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

func checkSplitDiffHasFileContext(t *testing.T, res *engine.Result) {
	t.Helper()
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
}

func checkScoutAndDeepReviewCounts(t *testing.T, res *engine.Result) {
	t.Helper()
	scouts, _ := res.State.Ctx["scout_files"].(map[string]any)
	if n, _ := scouts["count"].(int); n != 3 {
		t.Errorf("scout_files.count = %v, want 3 (one hermetic context per file)", scouts["count"])
	}
	deep, _ := res.State.Ctx["deep_review"].(map[string]any)
	if n, _ := deep["count"].(int); n != 2 {
		t.Errorf("deep_review.count = %v, want 2 (low-risk file filtered out)", deep["count"])
	}
}

func checkVerdictTransitionVetoed(t *testing.T, events []*journal.Event) {
	t.Helper()
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
	want := []string{"fetch_pr", "split_diff", "scout_files", "scout_pr", "note_trivial"}
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

// TestPRReviewLivePublish is the integration test for the live path with the
// GitHub API mocked out at the gh boundary (internal/ghfake). A real PR is
// present, so the machine FETCHES the diff (gh pr diff), runs the review on
// mocked models, and PUBLISHES the verdict back — comment + commit check. It
// asserts both the trace (fetch_pr … fetch_meta, post_comment, set_status) and
// the exact gh calls the machine made against the fake endpoint.
func TestPRReviewLivePublish(t *testing.T) {
	t.Chdir(t.TempDir())

	gh := ghfake.Install(t)
	diff, err := os.ReadFile(repoPath(t, "examples/pr-review/fixtures/pr.diff"))
	if err != nil {
		t.Fatal(err)
	}
	// The fake GitHub endpoint: `gh pr diff` yields the fixture; one `pr view`
	// JSON serves both fetch_pr (title/body) and fetch_meta (headSha, draft);
	// the comment returns its URL; the statuses API just acks.
	gh.Respond("pr diff", string(diff))
	gh.Respond("pr view", `{"number":123,"title":"queue: parallel worker pool",
		"body":"Process jobs concurrently","isDraft":false,
		"author":{"login":"alice","is_bot":false},
		"headRefName":"feat","headRefOid":"deadbeefcafe","baseRefName":"main",
		"labels":[],"additions":10,"deletions":2,"changedFiles":3}`)
	gh.Respond("pr comment", "https://github.com/o/r/pull/123#issuecomment-1\n")
	gh.Respond("api", "{}")

	m, script := loadExample(t, "pr-review")
	eng, store := newTestEngine(t, script)

	res, err := eng.Start(context.Background(), m, map[string]any{
		"pr":   "123",
		"repo": "o/r",
		"root": repoPath(t, "examples/pr-review/fixtures/repo"),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	// The trace now runs the live fetch and the publish tail.
	states, _, _ := eventTrace(t, store, res.RunID)
	want := []string{
		"fetch_pr", "split_diff", "scout_files", "scout_pr", "deep_review",
		"verdict", "write_review", "fetch_meta", "post_comment", "set_status",
	}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}

	// The machine fetched the diff live (not a passthrough).
	gh.WantCall("diff", "123", "--repo", "o/r")
	// It posted the verdict as a comment — the body carries the substantiated
	// findings from the mocked verdict.
	comment, ok := gh.FindCall("comment", "123")
	if !ok {
		t.Fatalf("no gh pr comment call; calls: %v", gh.Calls())
	}
	if !strings.Contains(strings.Join(comment, "\n"), "store.Find") {
		t.Errorf("comment body missing the verdict findings: %v", comment)
	}
	// And set a red commit check (the deep path has substantiated findings) on
	// the fetched head SHA.
	gh.WantCall("api", "repos/o/r/statuses/deadbeefcafe", "state=failure")
}

// TestParallelReviewTrace asserts the fork/join trace from
// examples/parallel-review/README.md: the parent enters only the fork and the
// join (branches run in child runs), the aggregate is label-keyed, and the
// three branch children are recorded under the parent.
func TestParallelReviewTrace(t *testing.T) {
	m, script := loadExample(t, "parallel-review")
	eng, store := newTestEngine(t, script)

	change, err := os.ReadFile(repoPath(t, "examples/parallel-review/fixtures/change.diff"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Start(context.Background(), m, map[string]any{"diff": string(change)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone || res.Terminal != "done" {
		t.Fatalf("status = %s at %s, want done at done", res.Status, res.Terminal)
	}

	// The parent journal is single-cursor: the passthrough front door, the fork,
	// and the join — never a branch state. With no `pr`, the live comment tail is
	// skipped.
	states, _, _ := eventTrace(t, store, res.RunID)
	want := []string{"fetch_pr", "review", "verdict"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("parent state sequence = %v, want %v (branches run in child runs)", states, want)
	}

	// The join read a label-keyed aggregate.
	agg, ok := res.State.Ctx["review"].(map[string]any)
	if !ok {
		t.Fatalf("ctx.review = %T, want the label-keyed aggregate", res.State.Ctx["review"])
	}
	for _, label := range []string{"security", "performance", "docs"} {
		if _, ok := agg[label].(map[string]any); !ok {
			t.Errorf("aggregate missing branch %q: %v", label, agg)
		}
	}
	if v, _ := res.State.Ctx["verdict"].(map[string]any); v["ship"] != true {
		t.Errorf("verdict.ship = %v, want true", v["ship"])
	}

	// Three branch children, hidden from the runs list but under the parent.
	kids, err := store.ListChildRuns(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 3 {
		t.Errorf("child runs = %d, want 3", len(kids))
	}
	top, err := store.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Errorf("ListRuns = %d, want 1 (children hidden)", len(top))
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
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	_, err = eng.Resume(context.Background(), m, res.RunID, "", nil)
	if err == nil {
		t.Error("resume without an event should error while parked")
	}
}

// TestMultiChoiceGate: a multi gate evaluates its options from scope at park
// time (journaled in run_parked), and the resume's {selected} lands in the
// gate's ctx entry, routing on the gate's single event edge.
func TestMultiChoiceGate(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const scan = { prompt: "scan", output: { modules: { type: "array", items: "string" } } };
const pick = {
  human: "Which modules should be regenerated?",
  choices: { multi: ({ scan }) => scan.modules, min: 1 },
};
const report = {
  write: "out/picked.txt",
  content: ({ pick }) => pick.selected.join(","),
};
export default {
  name: "multi-gate",
  model: "mock",
  states: { scan, pick, report },
  flow: pipe(scan, branch(pick, { chosen: report })),
};`
	script := `
scan:
  - text: '{"modules": ["auth", "billing", "search"]}'
`
	scriptPath := filepath.Join(t.TempDir(), "mock.yaml")
	err := os.WriteFile(scriptPath, []byte(script), 0o600)
	if err != nil {
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
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}

	checkMultiChoiceOptions(t, res.State.Parked.Choices)

	res2, err := eng.Resume(context.Background(), m, res.RunID, "chosen",
		map[string]any{"selected": []any{"auth", "search"}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res2.Status, res2.Terminal)
	}
	raw, err := os.ReadFile("out/picked.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "auth,search" {
		t.Errorf("picked.txt = %q — the selection should flow to downstream states", raw)
	}
}

func checkMultiChoiceOptions(t *testing.T, c *journal.ParkChoices) {
	t.Helper()
	if c == nil || c.Kind != "multi" || c.Event != "chosen" || c.Min != 1 {
		t.Fatalf("parked choices = %+v, want multi/chosen/min 1 (event defaulted from the single edge)", c)
	}
	if len(c.Options) != 3 || c.Options[0].Value != "auth" || c.Options[2].Value != "search" {
		t.Errorf("options = %+v, want the scope-evaluated module list", c.Options)
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
	// Every coder visit enters through its distill chain: generate#spec
	// slices the spec per file, generate#build_cause is empty (no build yet).
	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{
		"plan",
		"generate#spec", "generate#build_cause", "generate", "review",
		"generate#spec", "generate#build_cause", "generate", "review",
		"write_files", "build", "report",
	}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	// 12 journal events, but the 4 distill hops are implicit — the authored
	// topology still spends exactly 8 of the maxTransitions budget.
	if transitions != 12 {
		t.Errorf("journaled transitions = %d, want 12 (8 authored + 4 implicit)", transitions)
	}
	if res.State.Transitions != 8 {
		t.Errorf("counted transitions = %d, want 8 (distill hops are free)", res.State.Transitions)
	}

	// The fixture spec (~180 tokens) already fits the 400-token slice budget,
	// so every visit passes it through verbatim — zero model calls, and the
	// coder items carry the full spec.
	checkGenerateSpecPassthrough(t, res, string(spec))
	// No build has run on this trace: the absent source distilled to "".
	checkBuildCauseDistilledEmpty(t, res)

	// Gate two really executed the generated test: its exit code is DATA.
	checkBuildGateRanForReal(t, res)

	gen, _ := res.State.Ctx["generate"].(map[string]any)
	if n, _ := gen["count"].(int); n != 2 {
		t.Errorf("generate.count = %v, want 2 (one hermetic context per planned file)", gen["count"])
	}

	// The approved code reached disk and runs for real.
	checkCodegenArtifacts(t)
}

func checkGenerateSpecPassthrough(t *testing.T, res *engine.Result, spec string) {
	t.Helper()
	slices, _ := res.State.Ctx["generate#spec"].(map[string]any)
	if slices["passthrough_hits"] != 2 {
		t.Errorf("generate#spec.passthrough_hits = %v, want 2 (source fits the slice budget)", slices["passthrough_hits"])
	}
	items, ok := slices["items"].([]any)
	if !ok || len(items) != 2 {
		t.Errorf("generate#spec.items = %v, want 2", slices["items"])
		return
	}
	if first, _ := items[0].(map[string]any); first["text"] != spec {
		t.Errorf("generate#spec item 0 should be the verbatim spec, got %.80q", first["text"])
	}
}

func checkBuildCauseDistilledEmpty(t *testing.T, res *engine.Result) {
	t.Helper()
	causes, _ := res.State.Ctx["generate#build_cause"].(map[string]any)
	items, ok := causes["items"].([]any)
	if !ok || len(items) != 2 {
		t.Errorf("generate#build_cause.items = %v, want 2 empty slices", causes["items"])
		return
	}
	for i, it := range items {
		if m, _ := it.(map[string]any); m["text"] != "" {
			t.Errorf("generate#build_cause item %d = %v, want \"\" (absent source)", i, m["text"])
		}
	}
}

func checkBuildGateRanForReal(t *testing.T, res *engine.Result) {
	t.Helper()
	build, _ := res.State.Ctx["build"].(map[string]any)
	if build["ok"] != true || build["exit_code"] != 0 {
		t.Fatalf("build gate = %v, want ok:true exit:0 (test actually ran)", build)
	}
	if out, _ := build["stdout"].(string); !strings.Contains(out, "all tests passed") {
		t.Errorf("build stdout = %q, want the generated test's own output", build["stdout"])
	}
}

func checkCodegenArtifacts(t *testing.T) {
	t.Helper()
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

// incidentServers serves the incident-runbook fixtures over real HTTP —
// actions are never mocked. Paths under /v2/ require the auth header the
// machine sends via http.get's headers arg. The second URL is a
// deterministic connection-refused endpoint (server closed immediately).
func incidentServers(t *testing.T) (baseURL, deadURL string) {
	t.Helper()
	docroot := repoPath(t, "examples/incident-runbook/fixtures/serve")
	files := http.FileServer(http.Dir(docroot))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v2/") && r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // closed on purpose: connection refused, deterministically
	return srv.URL, dead.URL
}

const incidentText = "Honeybadger fault #83214792 (production): Redis::TimeoutError — Connection timed out after 5000ms in CheckoutController#create"

func incidentInputs(baseURL, faultURL string) map[string]any {
	return map[string]any{
		"incident":    incidentText,
		"services":    "api,worker,cache,search",
		"status_base": baseURL + "/status",
		"fault_url":   faultURL,
		"hb_auth":     "Bearer test-token",
	}
}

// TestIncidentRunbookEscalationTrace: the tracker is down (dead fault_url),
// so the run dead-letters it, the responder diagnoses on thin evidence at
// low confidence, the auditor flags it, and the senior ADOPTS the
// responder's actual conversation (lastTurns:2 trims the primer away).
func TestIncidentRunbookEscalationTrace(t *testing.T) {
	t.Chdir(t.TempDir())

	m, script := loadExample(t, "incident-runbook")
	eng, store := newTestEngine(t, script)
	baseURL, deadURL := incidentServers(t)

	res, err := eng.Start(context.Background(), m, incidentInputs(baseURL, deadURL))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusParked || res.State.Current != "pick" {
		t.Fatalf("status = %s at %s, want parked at pick", res.Status, res.State.Current)
	}

	checkIncidentProbes(t, res)
	checkTrackerDeadLetter(t, store, res.RunID)
	checkIncidentAdoptTrim(t, res)
	checkMultiRemediationGate(t, res.State.Parked.Choices)

	// NOTE: the engine does not enforce min: on Resume — tests pass
	// well-formed selections; the CLI/web surfaces enforce it.
	res2, err := eng.Resume(context.Background(), m, res.RunID, "chosen", map[string]any{
		"selected": []any{"Restart the cache cluster", "Enable request coalescing on api"},
		"note":     "hold the scale-up",
	})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res2.Status, res2.Terminal)
	}

	states, _, transitions := eventTrace(t, store, res.RunID)
	want := []string{"probe", "fetch_fault", "note_tracker", "responder", "verify", "take_over", "propose", "pick", "apply", "report"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 10 {
		t.Errorf("transitions = %d, want 10", transitions)
	}
	if apply, _ := res2.State.Ctx["apply"].(map[string]any); apply["count"] != 2 {
		t.Errorf("apply.count = %v, want 2 (one per selected remediation)", apply["count"])
	}
	checkReportContains(t, "83214792", "Restart the cache cluster", "hold the scale-up", "coalescing", "UNREACHABLE")
}

func checkIncidentProbes(t *testing.T, res *engine.Result) {
	t.Helper()
	// Probes ran for real: 4 items, the unknown service is 404-as-data,
	// the cache status file was actually served.
	probe, _ := res.State.Ctx["probe"].(map[string]any)
	if n, _ := probe["count"].(int); n != 4 {
		t.Fatalf("probe.count = %v, want 4", probe["count"])
	}
	items, _ := probe["items"].([]any)
	if last, _ := items[3].(map[string]any); last["status"] != 404 {
		t.Errorf("search probe status = %v, want 404 (non-2xx is DATA)", items[3])
	}
	if cache, _ := items[2].(map[string]any); !strings.Contains(cache["body"].(string), "down") {
		t.Errorf("cache probe body = %v, want the served fixture", items[2])
	}
}

func checkTrackerDeadLetter(t *testing.T, store journal.Store, runID string) {
	t.Helper()
	// The dead tracker dead-lettered: fetch_fault -> note_tracker via catch.
	_, statErr := os.Stat("out/tracker-failure.md")
	if statErr != nil {
		t.Errorf("tracker dead-letter note missing: %v", statErr)
	}
	_, failed, _ := eventTrace(t, store, runID)
	if failed["action_error"] != 1 || failed["rate_limited"] != 1 || failed["schema_violation"] != 1 {
		t.Errorf("failures by class = %v, want action_error:1 rate_limited:1 schema_violation:1", failed)
	}
}

func checkIncidentAdoptTrim(t *testing.T, res *engine.Result) {
	t.Helper()
	// Rung 2: the auditor's prompt carries the responder's transcript,
	// failed attempt included.
	verifyConvo := res.State.Convos["verify"]
	if len(verifyConvo) == 0 || !strings.Contains(verifyConvo[0].Text, "the cache looks sus") {
		t.Error("verify's prompt should embed the responder's transcript, failed attempt included")
	}
	// Rung 3 + trim: the responder's transcript is 4 messages
	// [prompt, bad reply, feedback, good JSON]; lastTurns:2 adopts only the
	// last exchange, so the senior's conversation is 2 adopted + its own
	// prompt + reply = 4 (untrimmed would be 6). The incident marker appears
	// exactly once — in the senior's OWN prompt.
	if n := len(res.State.Convos["responder"]); n != 4 {
		t.Fatalf("responder conversation = %d messages, want 4", n)
	}
	seniorConvo := res.State.Convos["take_over"]
	if len(seniorConvo) != 4 {
		t.Fatalf("take_over conversation = %d messages, want 4 (2 adopted + prompt + reply)", len(seniorConvo))
	}
	if !strings.Contains(seniorConvo[0].Text, "did not satisfy the output contract") {
		t.Errorf("adopted head = %q, want the responder's feedback exchange (the trimmed tail)", seniorConvo[0].Text)
	}
	count := 0
	for _, msg := range seniorConvo {
		count += strings.Count(msg.Text, "83214792")
	}
	if count != 1 {
		t.Errorf("incident marker appears %d times in the senior's conversation, want exactly 1 (trim dropped the primer)", count)
	}
}

func checkMultiRemediationGate(t *testing.T, ch *journal.ParkChoices) {
	t.Helper()
	if ch == nil || ch.Kind != "multi" || ch.Event != "chosen" || ch.Min != 1 || len(ch.Options) != 3 {
		t.Fatalf("parked choices = %+v, want multi/chosen/min 1 with 3 options", ch)
	}
	if ch.Options[0].Value != "Restart the cache cluster" {
		t.Errorf("option 0 = %q, want the first scripted remediation", ch.Options[0].Value)
	}
}

func checkReportContains(t *testing.T, want ...string) {
	t.Helper()
	report, err := os.ReadFile("out/incident-report.md")
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	for _, wantStr := range want {
		if !strings.Contains(string(report), wantStr) {
			t.Errorf("incident-report.md missing %q", wantStr)
		}
	}
}

// TestIncidentRunbookFastPathReportAnyway: the tracker is reachable, the
// responder is confident first try, the auditor agrees (no escalation), and
// a mis-drafted apply step routes catch:"*" straight to the report — which
// ships anyway with the "not drafted" fallback.
func TestIncidentRunbookFastPathReportAnyway(t *testing.T) {
	t.Chdir(t.TempDir())

	m, _ := loadExample(t, "incident-runbook")
	eng, store := newTestEngine(t, repoPath(t, "examples/incident-runbook/mock_fast_path.yaml"))
	baseURL, _ := incidentServers(t)

	faultURL := baseURL + "/v2/projects/8412/faults/83214792"
	res, err := eng.Start(context.Background(), m, incidentInputs(baseURL, faultURL))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}

	// The fault fetch succeeded THROUGH the auth middleware — the headers
	// arg works end to end over real HTTP.
	fault, _ := res.State.Ctx["fetch_fault"].(map[string]any)
	if fault["status"] != 200 {
		t.Fatalf("fetch_fault.status = %v, want 200", fault["status"])
	}
	if body, _ := fault["body"].(string); !strings.Contains(body, "Redis::TimeoutError") {
		t.Errorf("fetch_fault.body should be the served fixture, got %.80q", body)
	}

	res2, err := eng.Resume(context.Background(), m, res.RunID, "chosen",
		map[string]any{"selected": []any{"Restart the cache cluster"}})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res2.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res2.Status, res2.Terminal)
	}

	// No escalation, no dead letter — and apply's schema violation routed
	// catch:"*" straight to the report.
	states, failed, transitions := eventTrace(t, store, res.RunID)
	want := []string{"probe", "fetch_fault", "responder", "verify", "propose", "pick", "apply", "report"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	if transitions != 8 {
		t.Errorf("transitions = %d, want 8", transitions)
	}
	// retry:"none" still double-journals the exhausted semantic failure: the
	// in-conversation retry layer and withRetries both record it.
	if failed["schema_violation"] != 2 {
		t.Errorf("schema_violation failures = %d, want 2 (conversation layer + withRetries)", failed["schema_violation"])
	}

	checkVerifyFiredOnSound(t, store, res.RunID)
	checkApplyRoutedByCatch(t, store, res.RunID)
	checkReportContains(t, "83214792", "not drafted", "cache cluster down", "Fault detail fetched")
}

func checkVerifyFiredOnSound(t *testing.T, store journal.Store, runID string) {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != journal.TransitionFired {
			continue
		}
		if from, _ := ev.Data["from"].(string); from != "verify" {
			continue
		}
		if on, _ := ev.Data["on"].(string); on != "sound" {
			t.Errorf("verify fired on %q, want sound (guard passed)", on)
		}
	}
}

func checkApplyRoutedByCatch(t *testing.T, store journal.Store, runID string) {
	t.Helper()
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Type != journal.TransitionFired {
			continue
		}
		if from, _ := ev.Data["from"].(string); from != "apply" {
			continue
		}
		to, _ := ev.Data["to"].(string)
		on, _ := ev.Data["on"].(string)
		if to != "report" || on != "catch:schema_violation" {
			t.Errorf("apply routed to %q on %q, want report on catch:schema_violation", to, on)
		}
	}
}
