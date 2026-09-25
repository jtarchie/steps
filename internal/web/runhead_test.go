package web

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// One run with every section the page can point at, so the seam between the head and the body is the thing under test.
func startAuditedRun(t *testing.T, pipeline *Pipeline, runID string) {
	t.Helper()

	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, runID, []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1, Status: "succeeded"},
	})

	hash := strings.Repeat("b", 64)
	mustRecordResult(t, pipeline, hash, map[string]any{"response": "fine"})

	cost := 1.2

	err = pipeline.Store.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: runID, StepIndex: 0, StepName: "review", JobName: "build",
		NodeHash: hash, ModelReq: "opus", Total: 412_000, Cached: 250_000, CostUSD: &cost, FinishReason: "end_turn",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	err = pipeline.Store.RecordPlacement(ctx, store.Placement{
		RunID: runID, StepIndex: 0, StepName: "review", JobName: "build",
		NodeHash: hash, Slot: hash, Tag: "gpu", Address: "ssh://jt@box", GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/steps/work", FSType: "ext4",
	})
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

var inPageLink = regexp.MustCompile(`href="#([^"]+)"`)

// The head is an index of what sits below the transcript, and an index entry that names nothing is worse than none: a click that scrolls to the top silently. So every in-page link on the page names an id the page draws, and the sections it points at come AFTER the transcript, which is what puts the first step on the first screen.
func TestTheHeadPointsAtTheSectionsBelowTheTranscript(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startAuditedRun(t, pipeline, "run-audited")

	code, body := get(t, server, "/p/demo/runs/run-audited")
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	for _, match := range inPageLink.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `id="`+match[1]+`"`) {
			t.Errorf("the page links #%s and draws no such id", match[1])
		}
	}

	transcript := strings.Index(body, `id="transcript"`)
	spend := strings.Index(body, `id="spend"`)
	machines := strings.Index(body, `id="machines"`)

	if transcript < 0 || spend < transcript || machines < spend {
		t.Errorf("sections are not below the transcript in order (transcript %d, spend %d, machines %d)", transcript, spend, machines)
	}

	if !strings.Contains(body, `spend 412,000 tokens · 60% cached · $1.20 <a href="#spend">→ #spend</a>`) {
		t.Errorf("the head does not summarise the spend and point at the table:\n%s", body)
	}

	if !strings.Contains(body, `1 placed <a href="#machines">→ #machines</a>`) {
		t.Errorf("the head does not count the placements and point at the table:\n%s", body)
	}
}

// A run that called no model and left this machine has nothing to point at, and the head must not promise a section that is not there.
func TestTheHeadPointsAtNothingARunDoesNotHave(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-plain", "build", "succeeded")

	_, body := get(t, server, "/p/demo/runs/run-plain")

	for _, absent := range []string{`href="#spend"`, `id="spend"`, `href="#machines"`, `id="machines"`} {
		if strings.Contains(body, absent) {
			t.Errorf("a run with no spend or placements still carries %s", absent)
		}
	}
}

// The first line answers what happened and where; the second says what it ran against and what it cost. A reader triaging reads the first and stops, so nothing from the second may be on it.
func TestTheHeadReadsOutcomeThenProvenance(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "run-lines", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-lines", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "boom", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "boom", StepKind: "task", StepID: 1, Status: "failed", Text: "exit 1"},
		{Type: events.TypeJobFinished, Status: "failed", Text: "step 1 (task boom): exit 1"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-lines", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-lines")

	lines := regexp.MustCompile(`(?s)<p class="metaline[^"]*">(.*?)</p>`).FindAllStringSubmatch(body, -1)
	if len(lines) != 2 {
		t.Fatalf("the head has %d metalines, want outcome then provenance:\n%s", len(lines), body)
	}

	outcome, provenance := lines[0][1], lines[1][1]

	if !strings.Contains(outcome, "failed at") || strings.Contains(outcome, "/tmp/ws") {
		t.Errorf("the outcome line is not outcome only:\n%s", outcome)
	}

	if !strings.Contains(provenance, "/tmp/ws") || strings.Contains(provenance, "failed at") {
		t.Errorf("the provenance line is not provenance only:\n%s", provenance)
	}
}
