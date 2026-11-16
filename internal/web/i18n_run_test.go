package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// runPseudoFixture is a finished run holding what the run and node pages draw chrome for: an agent that called a tool, compacted and answered, a failed task, a step that was skipped, output, and a request that was slow against the step's timeout. Its data is one word each, so it can be told apart from chrome.
func runPseudoFixture(t *testing.T) (*Server, string) {
	t.Helper()

	server, pipeline, sha := tookPipeline(t)
	server.pseudo = true

	ctx := t.Context()
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "fxrun", JobName: "review", Workspace: t.TempDir(), ConfigSHA: sha})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	user := reviewerRow(t0, events.TypeAgentUser, 0)
	user.Text = "alpha"
	call := reviewerRow(t0, events.TypeAgentCall, 2*time.Second)
	call.Name, call.Detail = "grep", `{"pattern":"bravo"}`
	result := reviewerRow(t0, events.TypeAgentResult, 3*time.Second)
	result.Name, result.Detail = "grep", `{"hit":"charlie"}`
	slow := reviewerRow(t0, events.TypeAgentText, 134*time.Second)
	slow.Text = "delta"
	compact := reviewerRow(t0, events.TypeAgentCompaction, 135*time.Second)
	compact.Name, compact.Text = "echo", "foxtrot"
	finished := reviewerRow(t0, events.TypeStepFinished, 140*time.Second)
	finished.Status, finished.Hash = "succeeded", "beef0173"

	appendEvents(t, pipeline.Store, "fxrun", []store.RunEventRow{
		reviewerRow(t0, events.TypeStepStarted, 0), user, call, result, slow, compact, finished,
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "sierra", StepKind: "task", StepID: 2, Status: "failed", Text: "kaboom", At: t0.Add(150 * time.Second)},
		{Type: events.TypeStepOutput, StepIndex: 1, StepName: "sierra", StepKind: "task", StepID: 2, Text: "hotel\n", At: t0.Add(149 * time.Second)},
		{Type: events.TypeStepSkipped, StepIndex: 2, StepName: "tango", StepKind: "get", StepID: 3, Status: "skipped", Hash: "cafe1234567", Text: "juliet", At: t0.Add(151 * time.Second)},
	})
	mustRecordResult(t, pipeline, "beef0173", map[string]any{"response": "echo", "wrapped_up": true, "compactions": 1})

	err = pipeline.Store.FinishRun(ctx, "fxrun", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	return server, "alpha bravo charlie delta echo foxtrot sierra hotel tango juliet kaboom grep reviewer review agent task get across cafe pattern hit fxrun " + t.Name()
}

func TestTheRunAndNodePagesSpeakOnlyThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, fixture := runPseudoFixture(t)

	for _, path := range []string{"/p/demo/runs/fxrun", "/p/demo/nodes/beef0173"} {
		code, page := getAs(t, server, path, http.Header{"Accept-Language": {pseudoTag}})
		if code != http.StatusOK {
			t.Fatalf("%s: %d", path, code)
		}

		if left := untranslated(withoutHashes(page), fixture); len(left) > 0 {
			t.Errorf("%s: plain English outside the catalog: %v", path, slices.Compact(slices.Sorted(slices.Values(left))))
		}
	}
}

// TestTheLiveStreamSpeaksTheReadersLanguage: the stream draws the same step template as the page, per connection, so its frames carry the locale of the request that opened it.
func TestTheLiveStreamSpeaksTheReadersLanguage(t *testing.T) {
	t.Parallel()

	server, fixture := runPseudoFixture(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/p/demo/runs/fxrun/events", nil)
	req.Header.Set("Accept-Language", pseudoTag)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	frames := sseHTML(rec.Body.String())
	if !strings.Contains(frames, "stephead") {
		t.Fatalf("the stream drew no step:\n%s", rec.Body.String())
	}

	if left := untranslated(withoutHashes("<body>"+htmlLines(frames)), fixture); len(left) > 0 {
		t.Errorf("plain English in the stream's frames: %v", slices.Compact(slices.Sorted(slices.Values(left))))
	}
}

var hashLink = regexp.MustCompile(`class="hash"[^>]*>[^<]*<`)

// withoutHashes drops the short hashes a page links: they are data, and hex has words in it.
func withoutHashes(page string) string { return hashLink.ReplaceAllString(page, "><") }

// htmlLines keeps the stream lines that carry markup; the closing frame's JSON (icon, title) is not a row.
func htmlLines(frames string) string {
	var out []string

	for _, line := range strings.Split(frames, "\n") {
		if strings.Contains(line, "<") && !strings.HasPrefix(line, "{") {
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}
