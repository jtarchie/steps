package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

// recorder captures agent messages so tests can assert what a consumer state
// was actually shown — never LLM content, only machine-assembled context.
type recorder struct {
	engine.NopListener
	mu   sync.Mutex
	user map[string][]string // state -> user messages, in order
}

func (r *recorder) AgentMessage(state, role, text string) {
	if role != "user" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.user == nil {
		r.user = map[string][]string{}
	}
	r.user[state] = append(r.user[state], text)
}

func writeScript(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mock.yaml")
	err := os.WriteFile(path, []byte(script), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// longSource builds a source comfortably past the pass-through threshold
// (default slice budget 512 tokens ≈ 2048 chars) with a greppable marker, so
// tests exercise real extraction, not the verbatim pass-through.
func longSource(marker string) string {
	return marker + " " + strings.Repeat("the quick brown fox jumps over the lazy dog. ", 80)
}

// TestDistillBasicTrace: the implicit state runs first, the consumer sees the
// slice (never the raw source), and the implicit hop does not count toward
// maxTransitions.
func TestDistillBasicTrace(t *testing.T) {
	wf := `
export default {
  name: "distill-basic",
  input: { article: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: {
    summarize: {
      distill: { article: { for: "the key claims only" } },
      prompt: ({ article }) => "Summarize:\n" + article,
    },
  },
};`
	script := `
"summarize#article":
  - text: "THE-SLICE"
summarize:
  - text: "a fine summary"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))
	rec := &recorder{}
	eng.Listener = rec

	raw := longSource("RAW-ARTICLE-MARKER")
	res, err := eng.Start(context.Background(), m, map[string]any{"article": raw})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	states, _, _ := eventTrace(t, store, res.RunID)
	want := []string{"summarize#article", "summarize"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}

	// The consumer saw the slice, not the payload.
	msgs := rec.user["summarize"]
	if len(msgs) != 1 || msgs[0] != "Summarize:\nTHE-SLICE" {
		t.Errorf("consumer prompt = %q, want the distilled slice interpolated", msgs)
	}
	for _, msg := range msgs {
		if strings.Contains(msg, "RAW-ARTICLE-MARKER") {
			t.Errorf("consumer prompt leaked the raw source: %q", msg)
		}
	}
	// The distiller saw NEED + the raw source.
	dmsgs := rec.user["summarize#article"]
	if len(dmsgs) != 1 || !strings.Contains(dmsgs[0], "NEED: the key claims only") ||
		!strings.Contains(dmsgs[0], "SOURCE:\n"+raw) {
		t.Errorf("distiller prompt = %.120q, want NEED + SOURCE", dmsgs)
	}

	// Implicit hop excluded: only summarize -> done counts.
	if res.State.Transitions != 1 {
		t.Errorf("transitions = %d, want 1 (the distill hop is not authored topology)", res.State.Transitions)
	}
}

// TestDistillForEachZip: a fan-out consumer gets one slice per item, zipped
// back by index from the inherited fan-out.
func TestDistillForEachZip(t *testing.T) {
	wf := `
const seed = {
  action: "diff.split",
  input: {
    diff: [
      "diff --git a/one.go b/one.go", "+a",
      "diff --git a/two.go b/two.go", "+b",
    ].join("\n"),
  },
};
const work = {
  forEach: { over: ({ seed }) => seed.files, as: "file" },
  distill: { guide: { from: "manual", for: ({ file }) => "guidance for " + file.path } },
  prompt: ({ file, guide }) => "work on " + file.path + " using " + guide,
};
export default {
  name: "zip",
  input: { manual: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: { seed, work },
};`
	script := `
"work#guide":
  - text: "G-ONE"
  - text: "G-TWO"
work:
  - text: "done one"
  - text: "done two"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, writeScript(t, script))
	rec := &recorder{}
	eng.Listener = rec

	res, err := eng.Start(context.Background(), m, map[string]any{"manual": longSource("MANUAL-MARKER")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	msgs := rec.user["work"]
	want := []string{"work on one.go using G-ONE", "work on two.go using G-TWO"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Errorf("per-item prompts = %v, want slices zipped by index %v", msgs, want)
	}
	// Each distiller item carried its own need against the shared source.
	dmsgs := rec.user["work#guide"]
	if len(dmsgs) != 2 || !strings.Contains(dmsgs[0], "guidance for one.go") ||
		!strings.Contains(dmsgs[1], "guidance for two.go") {
		t.Errorf("distiller prompts = %v, want per-item needs", dmsgs)
	}
}

// TestDistillPassthroughSmallSource: a source that already fits the slice
// budget crosses verbatim — the identity is the best possible extraction, so
// no model call happens (the mock has no queue for the distiller: any call
// would fail the run loudly).
func TestDistillPassthroughSmallSource(t *testing.T) {
	wf := `
const seed = {
  action: "diff.split",
  input: {
    diff: [
      "diff --git a/one.go b/one.go", "+a",
      "diff --git a/two.go b/two.go", "+b",
    ].join("\n"),
  },
};
const work = {
  forEach: { over: ({ seed }) => seed.files, as: "file" },
  distill: { guide: { from: "manual", for: ({ file }) => "guidance for " + file.path } },
  prompt: ({ file, guide }) => "work on " + file.path + " using " + guide,
};
export default {
  name: "passthrough",
  input: { manual: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: { seed, work },
};`
	script := `
work:
  - text: "done one"
  - text: "done two"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))
	rec := &recorder{}
	eng.Listener = rec

	res, err := eng.Start(context.Background(), m, map[string]any{"manual": "TINY MANUAL"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}

	// The consumer got the source verbatim; the distiller never spoke.
	msgs := rec.user["work"]
	want := []string{"work on one.go using TINY MANUAL", "work on two.go using TINY MANUAL"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Errorf("consumer prompts = %v, want the verbatim source %v", msgs, want)
	}
	if dmsgs := rec.user["work#guide"]; len(dmsgs) != 0 {
		t.Errorf("distiller made %d model calls on a pass-through source", len(dmsgs))
	}
	guide, _ := res.State.Ctx["work#guide"].(map[string]any)
	if guide["passthrough_hits"] != 2 {
		t.Errorf("work#guide.passthrough_hits = %v, want 2", guide["passthrough_hits"])
	}

	// The journal records the pass-through like it records memo.
	events, err := store.Events(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	for _, ev := range events {
		if ev.Type == journal.HandlerFinished {
			if s, _ := ev.Data["state"].(string); s == "work#guide" {
				flagged, _ = ev.Data["passthrough"].(bool)
			}
		}
	}
	if !flagged {
		t.Error("work#guide handler_finished should carry passthrough: true")
	}
}

// TestDistillMemoAcrossRuns: distillation is pure — a byte-identical
// (source, need) pair replays for zero tokens, and a stable slice cascades
// into a consumer memo hit.
func TestDistillMemoAcrossRuns(t *testing.T) {
	wf := `
export default {
  name: "distill-memo",
  input: { article: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: {
    summarize: {
      memo: true,
      distill: { article: { for: "the key claims only" } },
      prompt: ({ article }) => "Summarize:\n" + article,
    },
  },
};`
	script := `
"summarize#article":
  - text: "THE-SLICE"
summarize:
  - text: "a fine summary"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, _ := newTestEngine(t, writeScript(t, script))
	input := map[string]any{"article": longSource("MEMO-MARKER")}

	first, err := eng.Start(context.Background(), m, input)
	if err != nil || first.Status != journal.StatusDone {
		t.Fatalf("first run: %v (%v)", err, first)
	}
	if first.State.Usage.Total() == 0 {
		t.Fatal("first run should spend tokens")
	}

	second, err := eng.Start(context.Background(), m, input)
	if err != nil || second.Status != journal.StatusDone {
		t.Fatalf("second run: %v (%v)", err, second)
	}
	if got := second.State.Usage.Total(); got != 0 {
		t.Errorf("second run spent %d tokens, want 0 (distill memo hit cascades into the consumer's)", got)
	}
}

// TestDistillAbsentSourceYieldsEmpty: a distill source that has not executed
// on this run's path (loop feedback before the loop) yields an empty slice
// for free — no model call, like adopt:self on a first visit.
func TestDistillAbsentSourceYieldsEmpty(t *testing.T) {
	wf := `
const triage = { prompt: "pick a path", output: { x: "string" }, events: ["skip", "full"] };
const research = { prompt: "dig deep" };
const use = {
  distill: { findings: { from: "research", for: "the verdict only" } },
  prompt: ({ findings }) => "conclude with:" + (findings ? findings : "(nothing yet)"),
};
export default {
  name: "absent",
  models: { distiller: "mock" },
  model: "mock",
  states: { triage, research, use },
  flow: pipe(branch(triage, { skip: use, else: pipe(research, use) })),
};`
	script := `
triage:
  - text: '{"x": "a", "event": "skip"}'
use:
  - text: "concluded"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))
	rec := &recorder{}
	eng.Listener = rec

	res, err := eng.Start(context.Background(), m, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done", res.Status, res.Terminal)
	}
	states, _, _ := eventTrace(t, store, res.RunID)
	want := []string{"triage", "use#findings", "use"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v", states, want)
	}
	// The distiller never called its (unscripted) mock queue, spent nothing,
	// and the consumer saw the empty slice as falsy.
	if msgs := rec.user["use#findings"]; len(msgs) != 0 {
		t.Errorf("distiller made a model call on an absent source: %v", msgs)
	}
	if msgs := rec.user["use"]; len(msgs) != 1 || msgs[0] != "conclude with:(nothing yet)" {
		t.Errorf("consumer prompt = %v, want the empty-slice fallback", msgs)
	}
}

// TestMaxInputTokensBudget: the input cap classifies over-budget renders as
// budget_exceeded (never retried, routable by catch:), and a machine-wide
// cap never cascades onto implicit distill states — the distiller is the one
// place the big payload is supposed to appear. The slice budget must fit
// under the consumer's cap (validated at load), so it is explicit here.
func TestMaxInputTokensBudget(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const summarize = {
  distill: { article: { for: "the key claims only", maxTokens: 80 } },
  prompt: ({ article }) => "Summarize:\n" + article,
};
const note = { write: "out/over.txt", content: "input budget blown" };
export default {
  name: "input-budget",
  input: { article: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  defaults: { maxInputTokens: 100 },
  states: { summarize, note },
  flow: pipe(branch(summarize, { catch: { budget_exceeded: note }, else: done })),
};`
	// The distiller reads the whole long source (exempt from the default
	// cap) and returns a slice that still blows the consumer's 100-token cap.
	script := `
"summarize#article":
  - text: "` + strings.Repeat("slice ", 120) + `"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))

	res, err := eng.Start(context.Background(), m, map[string]any{"article": longSource("BUDGET-MARKER")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done via the budget_exceeded catch", res.Status, res.Terminal)
	}
	states, failed, _ := eventTrace(t, store, res.RunID)
	want := []string{"summarize#article", "summarize", "note"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v (distiller exempt, consumer capped)", states, want)
	}
	if failed["budget_exceeded"] != 1 {
		t.Errorf("budget_exceeded failures = %d, want 1 (exhaustion: never retried)", failed["budget_exceeded"])
	}
}

// TestInputBudgetAttribution: an input overflow names its biggest offenders —
// the destructured scope values, largest first — and trips before any model
// call (zero tokens spent; the mock never plays).
func TestInputBudgetAttribution(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const use = {
  maxInputTokens: 50,
  prompt: ({ big, small }) => "CONTEXT:\n" + big + "\n" + small,
};
export default {
  name: "attr",
  input: { big: { type: "string", required: true }, small: { type: "string", required: true } },
  model: "mock",
  states: { use },
  flow: pipe(branch(use, { catch: { budget_exceeded: done }, else: done })),
};`
	script := `
use:
  - text: "should never be reached"
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))

	res, err := eng.Start(context.Background(), m, map[string]any{
		"big":   strings.Repeat("alpha ", 200), // ~300 tokens, way over the 50 cap
		"small": "tiny",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done via the budget_exceeded catch", res.Status, res.Terminal)
	}

	events, err := store.Events(context.Background(), res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, ev := range events {
		if ev.Type == journal.HandlerFailed && ev.Data["class"] == "budget_exceeded" {
			msg, _ = ev.Data["error"].(string)
		}
	}
	if msg == "" {
		t.Fatal("no budget_exceeded handler_failed event recorded")
	}
	if !strings.Contains(msg, "exceeds maxInputTokens 50") {
		t.Errorf("error = %q, want the cap named", msg)
	}
	if !strings.Contains(msg, "largest inputs: big ~") {
		t.Errorf("error = %q, want attribution naming the largest input first", msg)
	}
	if res.State.Usage.InputTokens != 0 || res.State.Usage.OutputTokens != 0 {
		t.Errorf("usage = %+v, want zero tokens (the cap trips before any model call)", res.State.Usage)
	}
}

// TestDistillFailureRoutesConsumerCatch: a distiller failure is the
// consumer's failure — same catch edges, so the run routes instead of dying.
func TestDistillFailureRoutesConsumerCatch(t *testing.T) {
	t.Chdir(t.TempDir())

	wf := `
const summarize = {
  retry: "none",
  distill: { article: { for: "the key claims only" } },
  prompt: ({ article }) => "Summarize:\n" + article,
};
const note = { write: "out/note.txt", content: "distiller down, routed" };
export default {
  name: "distill-catch",
  input: { article: { type: "string", required: true } },
  models: { distiller: "mock" },
  model: "mock",
  states: { summarize, note },
  flow: pipe(branch(summarize, { catch: { provider_error: note }, else: done })),
};`
	script := `
"summarize#article":
  - error: provider_error
`
	m, err := machine.Parse([]byte(wf))
	if err != nil {
		t.Fatal(err)
	}
	eng, store := newTestEngine(t, writeScript(t, script))

	res, err := eng.Start(context.Background(), m, map[string]any{"article": longSource("CATCH-MARKER")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Status != journal.StatusDone {
		t.Fatalf("status = %s at %s, want done via the copied catch edge", res.Status, res.Terminal)
	}
	states, failed, _ := eventTrace(t, store, res.RunID)
	want := []string{"summarize#article", "note"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Errorf("state sequence = %v, want %v (the consumer never runs)", states, want)
	}
	if failed["provider_error"] != 1 {
		t.Errorf("provider_error failures = %d, want 1 (retry: none inherited)", failed["provider_error"])
	}
	_, err = os.ReadFile("out/note.txt")
	if err != nil {
		t.Errorf("catch artifact: %v", err)
	}
}
