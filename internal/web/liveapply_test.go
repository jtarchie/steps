package web

// A reader who watched the stream is looking at the page a reload draws.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestStreamAppliedToThePageIsTheReloadedPage is the seam the finer-grained
// stream crosses: the server picks a unit to send (a whole row, its shell, a
// turn, a child) and an attribute saying where it lands, and the page only
// ends up right if every unit lands where the server meant and nothing that
// changed was left unsent. The browser does the landing with htmx, which no
// Go test can run — so this plays the frames onto the page's markup the way
// htmx's swaps do, textually, and demands the result be what the page's own
// template draws from the same events, byte for byte — after EVERY message,
// not just the last. The last is not enough: a container's own close re-sends
// it whole, which quietly repairs a stale rollup, a missed output or a child
// appended twice under it, and the reader watched every one of those.
//
// The fixture has every shape the stream sends differently: a step that
// opens after the page was drawn (appended), one that opens under a container
// the reader has (appended inside it), a container whose rollup changes when
// a child finishes (its shell), a sub-agent's turns (the child's name, no
// step of their own), an agent that ends by repeating its answer (the turn
// the page drops), an output on a step already drawn, a step re-parented
// under a container that opened after it (retracted, then re-appended
// nested) — both when the container is APPENDED and when the reader already
// has it and it is morphed whole — and a job error that quotes a step's own,
// which changes what that step's row shows without an event on the step.
//
// Serial, because liveBatch is a package global: every batch size is tried,
// and one event per flush is the hardest case — every unit stands alone.
func TestStreamAppliedToThePageIsTheReloadedPage(t *testing.T) {
	for _, batch := range []int{1, 2, 500} {
		t.Run(fmt.Sprintf("batch=%d", batch), func(t *testing.T) {
			shrinkRunEventLimit(t, 5000, batch)

			server, pipeline := testPipeline(t)
			ctx := t.Context()
			runID := fmt.Sprintf("run-apply-%d", batch)

			err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
			if err != nil {
				t.Fatalf("StartRun: %v", err)
			}

			appendEvents(t, pipeline.Store, runID, eventsBeforeThePage())

			_, page := get(t, server, "/p/demo/runs/"+runID)
			after := regexp.MustCompile(`/events\?after=(\d+)"`).FindStringSubmatch(page)
			if after == nil {
				t.Fatalf("the page does not say what sequence it was drawn from:\n%s", page)
			}

			drawnUpTo, _ := strconv.ParseInt(after[1], 10, 64)

			appendEvents(t, pipeline.Store, runID, eventsAfterThePage())
			mustRecordResult(t, pipeline, "beef7654321", map[string]any{"response": "Looks fine.", "wrapped_up": true})

			err = pipeline.Store.FinishRun(ctx, runID, "failed")
			if err != nil {
				t.Fatalf("FinishRun: %v", err)
			}

			raw := streamOf(t, server, "/p/demo/runs/"+runID+"/events?after="+after[1])
			watched := replayAtEveryFlush(t, server, pipeline, runID, page, raw, drawnUpTo, batch)

			_, reload := get(t, server, "/p/demo/runs/"+runID)
			if got, want := transcriptOf(t, watched), transcriptOf(t, reload); got != want {
				t.Errorf("a reader who watched the whole stream is not looking at what a reload draws.\nwatched:\n%s\n\nreload:\n%s", got, want)
			}

			if batch == 1 {
				assertTurnsWereAppended(t, raw)
			}

			// The comparison is only worth anything if the stream sent
			// something a plain reload would not have drawn already.
			for _, want := range []string{"the sub-agent said this", "second attempt", "1eaf1eaf1eaf", "unchanged — replayed", `id="step-22-inner"`} {
				if !strings.Contains(watched, want) {
					t.Errorf("the watched page never shows %q", want)
				}
			}
		})
	}
}

// assertTurnsWereAppended: the turns that arrived while review was still
// running went over as appends, not as the row again — the case the finer
// unit exists for, and one equivalence alone cannot see. The row itself is
// sent whole exactly twice: on its finish, and for the turn that arrived
// after it. A third time means the first turn after the page was drawn did
// not know the page already had turns.
func assertTurnsWereAppended(t *testing.T, raw string) {
	t.Helper()

	if !strings.Contains(raw, `hx-swap-oob="beforeend:#step-1-review_body"`) {
		t.Errorf("a running agent's new turn re-sent the whole row instead of appending:\n%s", sseHTML(raw))
	}

	if got := strings.Count(raw, `<div id="step-1-review" hx-swap-oob="outerMorph"`); got != 2 {
		t.Errorf("review's row went over whole %d times, want its finish and the turn after it:\n%s", got, sseHTML(raw))
	}
}

// eventsBeforeThePage is what the reader's page was drawn from: an agent
// step already talking, and a container the reader holds with nothing in it
// yet.
func eventsBeforeThePage() []store.RunEventRow {
	return []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1},
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "review", StepID: 1, Text: "Reading the diff first."},
		{Type: events.TypeAgentCall, StepIndex: 0, StepName: "review", StepID: 1, Name: "read_file", Detail: `{"path":"main.go"}`},
		{Type: events.TypeStepStarted, StepIndex: 7, StepName: "block", StepKind: "do", StepID: 20},
	}
}

// eventsAfterThePage is everything the stream has to deliver — see the test
// for which shape each of these is there for.
func eventsAfterThePage() []store.RunEventRow {
	return []store.RunEventRow{
		{Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", StepID: 1, Name: "read_file", Detail: `{"ok":true}`},
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "helper", Status: "depth:1", Text: "the sub-agent said this"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "matrix", StepKind: "across", StepID: 2},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "cell-a", StepKind: "task", StepID: 3, ParentStepID: 2},
		// A bare CR inside: the parser folds it to a newline on reload, and
		// the stream has to hand the reader the same lines.
		{Type: events.TypeStepOutput, StepIndex: 2, StepName: "cell-a", StepKind: "task", StepID: 3, ParentStepID: 2, Text: "first\rattempt\n"},
		{Type: events.TypeStepOutput, StepIndex: 2, StepName: "cell-a", StepKind: "task", StepID: 3, ParentStepID: 2, Text: "second attempt\n"},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "cell-a", StepKind: "task", StepID: 3, ParentStepID: 2, Status: "failed", Text: "exit 1", Worker: "gpu (ssh://jt@box)"},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "cell-b", StepKind: "task", StepID: 4, ParentStepID: 2},
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "review", StepID: 1, Text: "Looks fine."},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: "beef7654321", DurationMS: 4200},
		// Nothing publishes a turn after its step's finish today; the
		// fold accepts one, and by then the page has dropped the turn
		// that repeats the answer, so an append would land after it.
		{Type: events.TypeAgentText, StepIndex: 0, StepName: "review", StepID: 1, Text: "One more thing."},
		{Type: events.TypeStepStarted, StepIndex: 2, StepName: "inner", StepKind: "do", StepID: 5, ParentStepID: 2},
		{Type: events.TypeStepStarted, StepIndex: 3, StepName: "leaf", StepKind: "task", StepID: 6, ParentStepID: 5},
		{Type: events.TypeStepFinished, StepIndex: 3, StepName: "leaf", StepKind: "task", StepID: 6, ParentStepID: 5, Status: "succeeded", Hash: "1eaf1eaf1eaf"},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "inner", StepKind: "do", StepID: 5, ParentStepID: 2, Status: "succeeded"},
		{Type: events.TypeStepFinished, StepIndex: 2, StepName: "cell-b", StepKind: "task", StepID: 4, ParentStepID: 2, Status: "succeeded"},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "matrix", StepKind: "across", StepID: 2, Status: "failed"},
		{Type: events.TypeStepStarted, StepIndex: 4, StepName: "orphan", StepKind: "task", StepID: 8, ParentStepID: 7},
		{Type: events.TypeStepFinished, StepIndex: 4, StepName: "orphan", StepKind: "task", StepID: 8, ParentStepID: 7, Status: "succeeded"},
		{Type: events.TypeStepSkipped, StepIndex: 4, StepName: "wrapper", StepKind: "try", StepID: 7, Status: "skipped", Text: "cached"},
		{Type: events.TypeStepSkipped, StepIndex: 5, StepName: "ship", StepKind: "put", StepID: 9, Status: "skipped", Hash: "cafe1234567", Text: "unchanged — replayed from cache"},
		// An agent drawn BEFORE its first turn: the reader's copy has no body
		// to append into, so the first turn has to re-send the row, and only
		// the second can be appended.
		{Type: events.TypeStepStarted, StepIndex: 6, StepName: "assist", StepKind: "agent", StepID: 10},
		{Type: events.TypeAgentText, StepIndex: 6, StepName: "assist", StepID: 10, Text: "Starting."},
		{Type: events.TypeAgentText, StepIndex: 6, StepName: "assist", StepID: 10, Text: "Still going."},
		{Type: events.TypeStepFinished, StepIndex: 6, StepName: "assist", StepKind: "agent", StepID: 10, Status: "succeeded"},
		// The reader HAS `block`; its grandchild lands first, as a root, and
		// the wrapper that hangs it under the block arrives after. Sending the
		// block whole again — a morph, since they have it — has to retract
		// the loose copy first.
		{Type: events.TypeStepSkipped, StepIndex: 8, StepName: "inner", StepKind: "task", StepID: 22, ParentStepID: 21, Status: "skipped", Text: "cached"},
		{Type: events.TypeStepSkipped, StepIndex: 8, StepName: "wrap", StepKind: "try", StepID: 21, ParentStepID: 20, Status: "skipped", Text: "cached"},
		{Type: events.TypeStepFinished, StepIndex: 7, StepName: "block", StepKind: "do", StepID: 20, Status: "succeeded"},
		// Names no step, yet changes a row: cell-a's own error line is dropped
		// once the job's error quotes it, so the flush that reads this has to
		// re-send cell-a and nothing else.
		{Type: events.TypeJobFinished, Status: "failed", Text: "step 2 (task cell-a): exit 1"},
	}
}

// replayAtEveryFlush plays the stream onto page and, at every sequence a
// flush ends on, demands the result equal what the page's own template draws
// from the events up to there. Not every message: a flush that decides
// nothing changed writes no message at all, and that is exactly the flush
// that has to be checked. The stream reads pages of liveBatch from the
// sequence the page was drawn at, so those are the sequences it has
// committed to.
func replayAtEveryFlush(
	t *testing.T, server *Server, pipeline *Pipeline, runID, page, raw string, drawnUpTo int64, batch int,
) string {
	t.Helper()

	run, recorded, nodes := recordedRun(t, pipeline, runID)
	frames := framesOf(raw)
	last := recorded[len(recorded)-1].Seq

	for at, row := range recorded {
		if row.Seq <= drawnUpTo || !flushEndsAt(row.Seq, drawnUpTo, batch, last) {
			continue
		}

		for len(frames) > 0 && frames[0].id <= row.Seq {
			page = applyStream(t, page, frames[0].html)
			frames = frames[1:]
		}

		drawn := transcriptOf(t, renderTranscript(t, server, buildRunView(run, recorded[:at+1], nodes)))
		if got := transcriptOf(t, page); got != drawn {
			t.Fatalf("after sequence %d (%s), a reader who watched the stream is not looking at what the page draws from the same events.\nwatched:\n%s\n\ndrawn:\n%s",
				row.Seq, row.Type, got, drawn)
		}
	}

	if len(frames) > 0 {
		t.Fatalf("the stream sent %d messages stamped past the last recorded event", len(frames))
	}

	return page
}

// flushEndsAt reports whether the stream, reading pages of batch from
// drawnUpTo, ends a flush on this sequence.
func flushEndsAt(seq, drawnUpTo int64, batch int, last int64) bool {
	return (seq-drawnUpTo)%int64(batch) == 0 || seq == last
}

// recordedRun reads back everything the reference rendering needs.
func recordedRun(t *testing.T, pipeline *Pipeline, runID string) (store.RunRow, []store.RunEventRow, map[string]store.NodeRow) {
	t.Helper()

	ctx := t.Context()

	recorded, err := pipeline.Store.RunEvents(ctx, runID, 0, runEventLimit)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	nodes, err := pipeline.Store.NodesByHash(ctx, hashesOf(recorded))
	if err != nil {
		t.Fatalf("NodesByHash: %v", err)
	}

	run, _, err := pipeline.Store.FindRunRow(ctx, runID)
	if err != nil {
		t.Fatalf("FindRunRow: %v", err)
	}

	return run, recorded, nodes
}

// frame is one unnamed SSE message: the id the browser would resume from,
// and the markup it carries.
type frame struct {
	id   int64
	html string
}

// framesOf splits an SSE body into its content messages, in order.
func framesOf(raw string) []frame {
	var out []frame

	for _, message := range strings.Split(raw, "\n\n") {
		if strings.Contains(message, "event: ") || !strings.HasPrefix(message, "id: ") {
			continue
		}

		id, _ := strconv.ParseInt(strings.TrimPrefix(strings.SplitN(message, "\n", 2)[0], "id: "), 10, 64)
		out = append(out, frame{id: id, html: sseHTML(message)})
	}

	return out
}

// renderTranscript draws a view's transcript the way the page does — the
// same `step` template over the same roots — for a reference the HTTP page
// cannot give at an arbitrary sequence.
func renderTranscript(t *testing.T, server *Server, view runView) string {
	t.Helper()

	out := framer{server: server, page: map[string]any{"Nav": navData{Current: "demo"}, "Run": view}}
	out.out.WriteString(`<div class="transcript" id="transcript">`)

	for _, root := range view.Roots {
		err := out.render("step", stepCtx{Page: out.page, Step: root})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
	}

	out.out.WriteString(`</div>`)

	return out.out.String()
}

// oobAttr is the attribute that says where a fragment lands.
var oobAttr = regexp.MustCompile(` hx-swap-oob="([^"]*)"`)

// applyStream plays one message's fragments onto page the way htmx's
// out-of-band swaps would, and fails on the one thing htmx does in silence: a
// swap at an id the page does not have.
func applyStream(t *testing.T, page, html string) string {
	t.Helper()

	for at := 0; at < len(html); {
		open := strings.Index(html[at:], "<")
		if open < 0 {
			break
		}

		open += at

		name := tagName(html[open:])
		end := closeOf(t, html, open, name)
		page = applyFragment(t, page, html[open:end], name)
		at = end
	}

	return page
}

// applyFragment lands one out-of-band fragment.
func applyFragment(t *testing.T, page, fragment, name string) string {
	t.Helper()

	tag := fragment[:strings.Index(fragment, ">")+1]

	oob := oobAttr.FindStringSubmatch(tag)
	if oob == nil {
		t.Fatalf("a top-level fragment says nothing about where it lands:\n%s", fragment)
	}

	style, target, _ := strings.Cut(oob[1], ":")
	if target == "" {
		id := regexp.MustCompile(` id="([^"]*)"`).FindStringSubmatch(tag)
		if id == nil {
			t.Fatalf("an out-of-band fragment names no id:\n%s", fragment)
		}

		target = "#" + id[1]
	}

	mark := `id="` + strings.TrimPrefix(target, "#") + `"`

	at := strings.Index(page, mark)
	if at < 0 {
		t.Fatalf("the stream swaps at %s, which the page does not have — htmx drops that in silence:\n%s", target, fragment)
	}

	open := strings.LastIndex(page[:at], "<")
	end := closeOf(t, page, open, tagName(page[open:]))

	clean := oobAttr.ReplaceAllString(fragment, "")

	switch style {
	case "outerMorph":
		if strings.Contains(tag, " hx-morph-skip-children") {
			// Attributes only: the reader's children stay where they are.
			shut := strings.Index(page[open:], ">") + 1
			opened := strings.Replace(clean[:strings.Index(clean, ">")+1], " hx-morph-skip-children", "", 1)

			return page[:open] + opened + page[open+shut:]
		}

		return page[:open] + clean + page[end:]
	case "beforeend":
		inner := clean[strings.Index(clean, ">")+1 : strings.LastIndex(clean, "</")]
		shut := end - len("</"+name+">")

		return page[:shut] + inner + page[shut:]
	case "delete":
		return page[:open] + page[end:]
	default:
		t.Fatalf("the stream uses a swap this test does not play: %q", oob[1])

		return page
	}
}

func tagName(markup string) string {
	name := markup[1:]
	if end := strings.IndexAny(name, " \t\r\n>"); end >= 0 {
		name = name[:end]
	}

	return name
}

// transcriptOf is the page's transcript with what two renders legitimately
// disagree on taken out: the placeholder (the stream appends after it, the
// page draws it last), running clocks, and the whitespace between tags.
func transcriptOf(t *testing.T, page string) string {
	t.Helper()

	transcript := elementAt(t, page, `id="transcript"`)

	at := strings.Index(transcript, `id="run-empty"`)
	if at >= 0 {
		open := strings.LastIndex(transcript[:at], "<")
		transcript = transcript[:open] + transcript[closeOf(t, transcript, open, "div"):]
	}

	transcript = timeText.ReplaceAllString(transcript, "<time></time>")
	transcript = regexp.MustCompile(`\s+`).ReplaceAllString(transcript, " ")

	return strings.ReplaceAll(transcript, "> <", "><")
}
