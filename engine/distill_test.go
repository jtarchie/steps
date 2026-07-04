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
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
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

	res, err := eng.Start(context.Background(), m, map[string]any{"article": "RAW ARTICLE BODY"})
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
		if strings.Contains(msg, "RAW ARTICLE BODY") {
			t.Errorf("consumer prompt leaked the raw source: %q", msg)
		}
	}
	// The distiller saw NEED + the raw source.
	dmsgs := rec.user["summarize#article"]
	if len(dmsgs) != 1 || !strings.Contains(dmsgs[0], "NEED: the key claims only") ||
		!strings.Contains(dmsgs[0], "SOURCE:\nRAW ARTICLE BODY") {
		t.Errorf("distiller prompt = %q, want NEED + SOURCE", dmsgs)
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

	res, err := eng.Start(context.Background(), m, map[string]any{"manual": "THE WHOLE MANUAL"})
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
	input := map[string]any{"article": "RAW ARTICLE BODY"}

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

	res, err := eng.Start(context.Background(), m, map[string]any{"article": "RAW"})
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
	if _, err := os.ReadFile("out/note.txt"); err != nil {
		t.Errorf("catch artifact: %v", err)
	}
}
