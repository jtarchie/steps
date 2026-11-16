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

// On a phone an agent's row puts its spend and its image on one line; the spend wraps, and the digest keeps every character.
func TestAPhoneKeepsAnAgentsDigestWhole(t *testing.T) {
	ctx := browser(t)
	server, pipeline := testPipeline(t)
	hash := strings.Repeat("e", 64)
	cost := 10.39

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "run-agent", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatal(err)
	}

	appendEvents(t, pipeline.Store, "run-agent", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: hash},
	})
	mustRecordResult(t, pipeline, hash, map[string]any{"response": "fine"})

	err = pipeline.Store.RecordAgentUsage(t.Context(), store.AgentUsage{
		RunID: "run-agent", StepIndex: 0, StepName: "reviewer", JobName: "build",
		NodeHash: hash, ModelReq: "opus", Total: 1_481_145, Cached: 1_461_436, CostUSD: &cost, FinishReason: "end_turn",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.RecordPlacement(t.Context(), store.Placement{
		RunID: "run-agent", StepIndex: 0, StepName: "reviewer", JobName: "build",
		NodeHash: hash, Slot: hash, Address: "local:", GOOS: "linux", GOARCH: "arm64", Image: "localhost:5050/backerkit@sha256:" + strings.Repeat("af8016ce", 8),
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.FinishRun(t.Context(), "run-agent", "succeeded")
	if err != nil {
		t.Fatal(err)
	}

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	cut := must(ctx, t, chromedp.Evaluate[string](`(() => {
  const img = document.querySelector('#transcript .stephead .note.worker .img');
  return img ? String(img.scrollWidth - img.clientWidth) : 'no image chip';
})()`), chromedp.EmulateViewport(375, 812), media("dark", false), chromedp.Navigate(listener.URL+"/p/demo/runs/run-agent"))

	if cut != "0" {
		t.Errorf("on a phone the agent row's digest chip is cut by %spx", cut)
	}
}
