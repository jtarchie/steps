package web

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// placedRun is a passed task step that ran in a digest-pinned image on the local worker, the shape every container step of a pr-review run has.
func placedRun(t *testing.T, pipeline *Pipeline, runID, image, worker string) {
	t.Helper()

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: runID, JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	hash := strings.Repeat("c", 64)

	appendEvents(t, pipeline.Store, runID, []store.RunEventRow{
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "checkout", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "checkout", StepKind: "task", StepID: 1, Status: "succeeded", Hash: hash, DurationMS: 17300, Worker: worker},
	})

	mustRecordResult(t, pipeline, hash, map[string]any{})

	err = pipeline.Store.RecordPlacement(t.Context(), store.Placement{
		RunID: runID, StepIndex: 0, StepName: "checkout", JobName: "build",
		NodeHash: hash, Slot: hash, Address: "local:", GOOS: "linux", GOARCH: "arm64", Image: image,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.FinishRun(t.Context(), runID, "succeeded")
	if err != nil {
		t.Fatal(err)
	}
}

// The step's name is what a reader scans a transcript for, so nothing in its row may be drawn over it: a long image reference beside it once ran straight into it ("checkoutlocalhost:5050/…").
func TestAStepNameIsNeverDrawnOver(t *testing.T) {
	ctx := browser(t)
	server, pipeline := testPipeline(t)
	placedRun(t, pipeline, "run-placed", "localhost:5050/backerkit@sha256:"+strings.Repeat("af8016ce", 8), "")

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	type nameLayout struct {
		Gap   float64 `json:"gap"`
		Lines int     `json:"lines"`
	}

	got := must(ctx, t, chromedp.Evaluate[nameLayout](`(() => {
  const head = document.querySelector('#transcript .stephead');
  const el = head.querySelector('.name');
  const name = el.getBoundingClientRect();
  const next = [...head.querySelectorAll('.note, .headmeta > *')].map(e => e.getBoundingClientRect()).filter(r => r.width > 0 && r.top < name.bottom && r.bottom > name.top);
  const range = document.createRange();
  range.selectNodeContents(el);
  const lines = new Set([...range.getClientRects()].map(r => Math.round(r.top))).size;
  return {gap: Math.min(...next.map(r => r.left)) - name.right, lines};
})()`), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs/run-placed"))

	if got.Gap < 0 {
		t.Errorf("the step name's right edge runs %.0fpx under the metadata beside it", -got.Gap)
	}

	// Wrapping is the other way to make room, and as bad: "check" over "out".
	if got.Lines != 1 {
		t.Errorf("the step name broke across %d lines to make room for the metadata beside it", got.Lines)
	}
}

// Short of room, the row cuts the worker's name before the image's digest: a digest cut short no longer tells two images apart, and the worker is named again in full on the step's placement line.
func TestARowCutsTheWorkerBeforeTheDigest(t *testing.T) {
	ctx := browser(t)
	server, pipeline := testPipeline(t)
	placedRun(t, pipeline, "run-placed", "localhost:5050/backerkit@sha256:"+strings.Repeat("af8016ce", 8), "gpu (aws://i-0abc123def4567890 in us-west-2 behind the bastion "+strings.Repeat("hop ", 30)+")")

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	cut := must(ctx, t, chromedp.Evaluate[string](`(() => {
  const img = document.querySelector('#transcript .stephead .note.worker .img');
  return img ? String(img.scrollWidth - img.clientWidth) : 'no image chip';
})()`), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs/run-placed"))

	if cut != "0" {
		t.Errorf("the digest chip beside a long worker name is cut by %spx", cut)
	}
}

// agentRun is a passed agent step with spend, placed in a digest-pinned image, the reviewer of a pr-review run.
func agentRun(t *testing.T, pipeline *Pipeline, runID string) {
	t.Helper()

	hash := strings.Repeat("e", 64)
	cost := 10.39

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: runID, JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	appendEvents(t, pipeline.Store, runID, []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: hash},
	})
	mustRecordResult(t, pipeline, hash, map[string]any{"response": "fine"})

	err = pipeline.Store.RecordAgentUsage(t.Context(), store.AgentUsage{
		RunID: runID, StepIndex: 0, StepName: "reviewer", JobName: "build",
		NodeHash: hash, ModelReq: "opus", Total: 1_481_145, Cached: 1_461_436, CostUSD: &cost, FinishReason: "end_turn",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.RecordPlacement(t.Context(), store.Placement{
		RunID: runID, StepIndex: 0, StepName: "reviewer", JobName: "build",
		NodeHash: hash, Slot: hash, Address: "local:", GOOS: "linux", GOARCH: "arm64", BytesReceived: 11_776,
		Image: "localhost:5050/backerkit@sha256:" + strings.Repeat("af8016ce", 8),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.FinishRun(t.Context(), runID, "succeeded")
	if err != nil {
		t.Fatal(err)
	}
}

// At every width an agent's row keeps its digest whole: the spend beside it wraps instead. It was once whole on a phone, which had its own rule, and cut at tablet widths, which had none.
func TestAnAgentsDigestIsWholeAtEveryWidth(t *testing.T) {
	ctx := browser(t)
	server, pipeline := testPipeline(t)
	agentRun(t, pipeline, "run-agent")

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	for _, width := range []int64{375, 700, 768, 900, 1024, 1280} {
		cut := must(ctx, t, chromedp.Evaluate[string](`(() => {
  const img = document.querySelector('#transcript .stephead .note.worker .img');
  return img ? String(img.scrollWidth - img.clientWidth) : 'no image chip';
})()`), chromedp.EmulateViewport(width, 900), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs/run-agent"))

		if cut != "0" {
			t.Errorf("at %dpx the agent row's digest chip is cut by %spx", width, cut)
		}
	}
}

// On a phone a step's spend and placement wrap only between their parts: "11.5" over "KiB back" or "98%" over "cached" is a number torn from its unit.
func TestAPhoneBreaksSpendAndPlacementOnlyBetweenParts(t *testing.T) {
	ctx := browser(t)
	server, pipeline := testPipeline(t)
	agentRun(t, pipeline, "run-agent")

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	for _, width := range []int64{320, 340, 360, 375, 390, 414} {
		torn := must(ctx, t, chromedp.Evaluate[[]string](`(() => {
  const parts = [...document.querySelectorAll('#transcript .note.usage .seg, #transcript .agentmeta .seg:not(.img)')];
  if (parts.length < 6) return ['only ' + parts.length + ' parts'];
  return parts.filter(e => {
    const range = document.createRange();
    range.selectNodeContents(e);
    return new Set([...range.getClientRects()].map(r => Math.round(r.top))).size > 1;
  }).map(e => e.textContent);
})()`), chromedp.EmulateViewport(width, 812), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs/run-agent"))

		if len(torn) > 0 {
			t.Errorf("at %dpx these parts break across lines: %q", width, torn)
		}
	}
}

// A folded prompt's speaker is a label beside its <summary>, which a reader tabbing from fold to fold never hears: the summary itself says who is speaking.
func TestAFoldedPromptSaysWhoIsSpeaking(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	hash := strings.Repeat("f", 64)

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "run-prompts", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	appendEvents(t, pipeline.Store, "run-prompts", []store.RunEventRow{
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeAgentSystem, StepIndex: 0, StepName: "reviewer", StepID: 1, Text: "Everything you write goes out under my name."},
		{Type: events.TypeAgentUser, StepIndex: 0, StepName: "reviewer", StepID: 1, Text: strings.Repeat("Review the staged change. ", 100)},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: hash},
	})
	mustRecordResult(t, pipeline, hash, map[string]any{"response": "fine"})

	err = pipeline.Store.FinishRun(t.Context(), "run-prompts", "succeeded")
	if err != nil {
		t.Fatal(err)
	}

	_, page := get(t, server, "/p/demo/runs/run-prompts")

	for _, who := range []string{"system", "user"} {
		if !strings.Contains(page, `<summary><span class="visually-hidden">`+who+`: </span>`) {
			t.Errorf("the folded %s prompt's summary does not name its speaker:\n%s", who, page)
		}
	}
}
