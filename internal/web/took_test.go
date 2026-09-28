package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/runview"
	"github.com/jtarchie/steps/internal/store"
)

// tookPipeline is one agent with a 10m timeout, so a request over 60s is slow.
func tookPipeline(t *testing.T) (*Server, *Pipeline, string) {
	t.Helper()

	server, pipeline := serverFromYAML(t, `
agents:
  - name: reviewer
    source: { model: openrouter/qwen/qwen3.7-flash }

jobs:
  - name: review
    plan:
      - agent: reviewer
        messages: ["go"]
        timeout: 10m
`)

	sha := pipeline.Config().Revision.SHA
	if sha == "" {
		t.Fatal("the loaded config carries no revision to match against")
	}

	return server, pipeline, sha
}

// reviewerRow is one event of the reviewer step at t0+offset.
func reviewerRow(t0 time.Time, eventType string, offset time.Duration) store.RunEventRow {
	return store.RunEventRow{Type: eventType, StepIndex: 0, StepName: "reviewer", StepKind: "agent", StepID: 1, At: t0.Add(offset)}
}

// turnsIn is every turn element in body, in order, the answer block included.
func turnsIn(t *testing.T, body string) []string {
	t.Helper()

	var out []string

	for at := 0; ; {
		next := strings.Index(body[at:], `<div class="turn `)
		if next < 0 {
			return out
		}

		open := at + next
		shut := closeOf(t, body, open, "div")
		out = append(out, body[open:shut])
		at = shut
	}
}

// TestRunPageShowsHowLongEachModelRequestTook is the issue's own reading: a
// request of 2s and one of 132s, the second flagged against a 10m timeout,
// and nothing on the turns the model did not write.
func TestRunPageShowsHowLongEachModelRequestTook(t *testing.T) {
	t.Parallel()

	server, pipeline, sha := tookPipeline(t)
	ctx := t.Context()
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	err := pipeline.Store.StartRun(ctx, "run-took", "review", t.TempDir(), sha)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	user := reviewerRow(t0, events.TypeAgentUser, 0)
	user.Text = "Review this."
	first := reviewerRow(t0, events.TypeAgentCall, 2*time.Second)
	first.Name = "read_file"
	result := reviewerRow(t0, events.TypeAgentResult, 2500*time.Millisecond)
	result.Name = "read_file"
	slow := reviewerRow(t0, events.TypeAgentText, 134500*time.Millisecond)
	slow.Text = "Thinking hard about it."
	second := reviewerRow(t0, events.TypeAgentCall, 134600*time.Millisecond)
	second.Name = "grep"
	secondResult := reviewerRow(t0, events.TypeAgentResult, 135*time.Second)
	secondResult.Name = "grep"
	answer := reviewerRow(t0, events.TypeAgentText, 140*time.Second)
	answer.Text = "Looks fine."
	finished := reviewerRow(t0, events.TypeStepFinished, 141*time.Second)
	finished.Status, finished.Hash = "succeeded", "beef0173"

	appendEvents(t, pipeline.Store, "run-took", []store.RunEventRow{
		reviewerRow(t0, events.TypeStepStarted, 0), user, first, result, slow, second, secondResult, answer, finished,
	})
	mustRecordResult(t, pipeline, "beef0173", map[string]any{"response": "Looks fine."})

	err = pipeline.Store.FinishRun(ctx, "run-took", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/run-took")

	assertRequestTimes(t, turnsIn(t, body))
}

// assertRequestTimes reads TestRunPageShowsHowLongEachModelRequestTook's page.
func assertRequestTimes(t *testing.T, turns []string) {
	t.Helper()

	if len(turns) != 7 {
		t.Fatalf("drew %d turns, want user, call, result, text, call, result, answer:\n%s", len(turns), strings.Join(turns, "\n---\n"))
	}

	if !strings.Contains(turns[1], `<span class="dur" title="sent 2026-09-24T12:00:00Z · answered 2026-09-24T12:00:02Z">2.0s</span>`) {
		t.Errorf("the first call does not carry its 2s request, unflagged:\n%s", turns[1])
	}

	if !strings.Contains(turns[3], `class="dur warn"`) || !strings.Contains(turns[3], ">2m 12s<") ||
		!strings.Contains(turns[3], `<span class="visually-hidden"> — slow request</span>`) {
		t.Errorf("the 132s request is not drawn as 2m 12s and flagged slow in words:\n%s", turns[3])
	}

	for _, i := range []int{0, 2, 4, 5} {
		if strings.Contains(turns[i], `class="dur`) {
			t.Errorf("turn %d is not the first model turn of a response, and carries a duration:\n%s", i, turns[i])
		}
	}

	if !strings.Contains(turns[6], `class="turn answer"`) || !strings.Contains(turns[6], ">5.0s<") {
		t.Errorf("the answer block does not carry its request's 5s:\n%s", turns[6])
	}
}

// TestTimedOutStepSaysHowLongTheModelHadTheLastRequest is the number the
// issue was opened for: the request the step died waiting on left no turn.
func TestTimedOutStepSaysHowLongTheModelHadTheLastRequest(t *testing.T) {
	t.Parallel()

	server, pipeline, sha := tookPipeline(t)
	ctx := t.Context()
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	opening := []store.RunEventRow{
		reviewerRow(t0, events.TypeStepStarted, 0),
		reviewerRow(t0, events.TypeAgentUser, 0),
		reviewerRow(t0, events.TypeAgentCall, time.Second),
		reviewerRow(t0, events.TypeAgentResult, 2*time.Second),
	}

	failedAt := func(row store.RunEventRow) store.RunEventRow {
		row.Status, row.Text = "failed", "context deadline exceeded"

		return row
	}

	for _, tc := range []struct {
		name, run string
		rows      []store.RunEventRow
		want      bool
	}{
		{"timed out mid-request", "run-cut", append(opening[:4:4],
			failedAt(reviewerRow(t0, events.TypeStepFinished, 2*time.Second+7*time.Minute+40*time.Second))), true},
		{"failed after the model answered", "run-answered", append(opening[:4:4],
			reviewerRow(t0, events.TypeAgentText, 3*time.Second),
			failedAt(reviewerRow(t0, events.TypeStepFinished, 4*time.Second))), false},
		{"still running", "run-open", opening, false},
	} {
		err := pipeline.Store.StartRun(ctx, tc.run, "review", t.TempDir(), sha)
		if err != nil {
			t.Fatalf("StartRun: %v", err)
		}

		appendEvents(t, pipeline.Store, tc.run, tc.rows)

		_, body := get(t, server, "/p/demo/runs/"+tc.run)

		drawn := strings.Contains(body, `class="turn pending"`)
		if drawn != tc.want {
			t.Errorf("%s: unanswered request drawn = %v, want %v", tc.name, drawn, tc.want)
		}

		if tc.want {
			pending := extractDiv(t, body, "turn pending")
			if !strings.Contains(pending, `class="dur warn"`) || !strings.Contains(pending, ">7m 40s<") ||
				!strings.Contains(pending, "step ended 2026-09-24T12:07:42Z") {
				t.Errorf("%s: the unanswered request does not say 7m 40s, flagged, ending with the step:\n%s", tc.name, pending)
			}
		}
	}
}

// TestStreamedTurnCarriesTheDurationThePageDraws: a turn appended to a live
// row carries the request time, and slow flag, a reload draws for it — which
// only holds if the stream's fold times the turn, not its template.
//
// Serial, because shrinkIdleTimeout writes a package global.
func TestStreamedTurnCarriesTheDurationThePageDraws(t *testing.T) {
	shrinkIdleTimeout(t, 300*time.Millisecond)

	server, pipeline, sha := tookPipeline(t)
	ctx := t.Context()
	t0 := time.Now().UTC().Truncate(time.Second)

	err := pipeline.Store.StartRun(ctx, "run-live-took", "review", t.TempDir(), sha)
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	slow := reviewerRow(t0, events.TypeAgentText, 134500*time.Millisecond)
	slow.Text = "Thinking hard about it."

	appendEvents(t, pipeline.Store, "run-live-took", []store.RunEventRow{
		// seq 1-3: on the reader's page when they connected.
		reviewerRow(t0, events.TypeStepStarted, 0),
		reviewerRow(t0, events.TypeAgentUser, 0),
		reviewerRow(t0, events.TypeAgentCall, 2*time.Second),
		// seq 4-5: appended to the live row.
		reviewerRow(t0, events.TypeAgentResult, 2500*time.Millisecond),
		slow,
	})

	raw := streamOf(t, server, "/p/demo/runs/run-live-took/events?after=3")
	stream := sseHTML(raw)

	if !strings.Contains(stream, `hx-swap-oob="beforeend:#step-1-reviewer_body"`) {
		t.Fatalf("the turns were not appended to the live row, so this test measures nothing:\n%s", stream)
	}

	_, page := get(t, server, "/p/demo/runs/run-live-took")

	drawn := extractDiv(t, page, "turn model")
	streamed := extractDiv(t, stream, "turn model")

	if streamed != drawn {
		t.Errorf("the streamed turn differs from the page's:\nstream: %s\npage:   %s", streamed, drawn)
	}

	if !strings.Contains(streamed, `class="dur warn"`) || !strings.Contains(streamed, ">2m 12s<") {
		t.Errorf("the streamed turn does not carry its flagged 2m 12s request:\n%s", streamed)
	}
}

// TestTimeoutSurvivesTheStepFinishing: the slow flag is read on a finished
// step, where the countdown is not; and a timeout that cannot be known is
// zero, never a stale or default one.
func TestTimeoutSurvivesTheStepFinishing(t *testing.T) {
	t.Parallel()

	_, pipeline, sha := timeoutPipelineSHA(t)
	cfg := pipeline.Config()

	view := runView{Transcript: runview.Transcript{Steps: []*stepView{
		{Name: "reviewer", Kind: "agent", Status: "succeeded", Started: time.Now()},
		{Name: "patient", Kind: "agent", Status: "succeeded", Started: time.Now()},
		{Name: "build", Kind: "task", Status: "succeeded", Started: time.Now()},
	}}}

	attachStepDeadlines(&view, cfg, "review", sha)

	reviewer, patient, build := view.Steps[0], view.Steps[1], view.Steps[2]

	if reviewer.Timeout != 30*time.Minute || reviewer.HasDeadline() {
		t.Errorf("finished reviewer: Timeout = %v, deadline %v; want the 30m default and no countdown", reviewer.Timeout, reviewer.Deadline)
	}

	if patient.Timeout != 0 || build.Timeout != 0 {
		t.Errorf("Timeout = %v (timeout: \"0\") and %v (task), want 0 for both", patient.Timeout, build.Timeout)
	}

	// The live fold reuses the pointer: drifting must clear what was set.
	attachStepDeadlines(&view, cfg, "review", "not-the-loaded-sha")

	if reviewer.Timeout != 0 {
		t.Errorf("a drifted config left Timeout = %v, want 0", reviewer.Timeout)
	}
}
