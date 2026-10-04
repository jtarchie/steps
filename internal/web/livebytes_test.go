package web

// What a watcher is sent over the life of a run.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// streamedBytes is what one connection receives for a run of one agent step
// that speaks `turns` times, with every turn delivered by a flush of its own —
// which is what a watcher of a live agent sees, one event per tick.
func streamedBytes(t *testing.T, turns int) int {
	t.Helper()

	server, pipeline := testPipeline(t)
	ctx := t.Context()
	runID := fmt.Sprintf("run-bytes-%d", turns)

	err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows := make([]store.RunEventRow, 0, 1+turns)
	rows = append(rows, store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1})

	for i := range turns {
		rows = append(rows, store.RunEventRow{
			Type: events.TypeAgentText, StepIndex: 0, StepName: "review", StepID: 1,
			Text: fmt.Sprintf("Turn %d: reading the next file and thinking about what it means for the review.", i),
		})
	}

	appendEvents(t, pipeline.Store, runID, rows)

	err = pipeline.Store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	return len(streamOf(t, server, "/p/demo/runs/"+runID+"/events?after=1"))
}

func TestStreamBytesGrowLinearlyWithTurns(t *testing.T) {
	shrinkRunEventLimit(t, 5000, 1)

	at50, at100, at200 := streamedBytes(t, 50), streamedBytes(t, 100), streamedBytes(t, 200)
	t.Logf("turns= 50  streamed=%9d bytes", at50)
	t.Logf("turns=100  streamed=%9d bytes", at100)
	t.Logf("turns=200  streamed=%9d bytes", at200)

	// The curve, not a byte count: doubling the turns must not much more than
	// double the bytes. Re-shipping the transcript on every turn quadruples
	// it (measured 3.7x and 3.9x before the fix), and a fixed per-flush cost
	// keeps a linear stream a little UNDER 2x — so 2.5 is far from both.
	for _, pair := range []struct{ small, big int }{{at50, at100}, {at100, at200}} {
		if ratio := float64(pair.big) / float64(pair.small); ratio > 2.5 {
			t.Errorf("doubling the turns multiplied the bytes by %.2f: the stream is re-shipping the transcript", ratio)
		}
	}
}

// streamedChunkBytes is what one connection receives for a task that prints `chunks` times before its record, each chunk a flush of its own.
func streamedChunkBytes(t *testing.T, chunks int) int {
	t.Helper()

	server, pipeline := testPipeline(t)
	ctx := t.Context()
	runID := fmt.Sprintf("run-chunks-%d", chunks)

	err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows := make([]store.RunEventRow, 0, 1+chunks)
	rows = append(rows, store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1})

	for i := range chunks {
		rows = append(rows, store.RunEventRow{
			Type: events.TypeStepOutputChunk, StepIndex: 0, StepName: "build", StepKind: "task", StepID: 1, Status: "stdout",
			Text: fmt.Sprintf("line %d: compiled another file and linked it against the rest of the tree\n", i),
		})
	}

	appendEvents(t, pipeline.Store, runID, rows)

	err = pipeline.Store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	return len(streamOf(t, server, "/p/demo/runs/"+runID+"/events?after=1"))
}

// TestStreamBytesGrowLinearlyWithChunks is the printing step's version of the curve: an output event re-sends its whole row, so a chunk sent that way ships every chunk before it, again.
func TestStreamBytesGrowLinearlyWithChunks(t *testing.T) {
	shrinkRunEventLimit(t, 5000, 1)

	at50, at100, at200 := streamedChunkBytes(t, 50), streamedChunkBytes(t, 100), streamedChunkBytes(t, 200)
	t.Logf("chunks= 50  streamed=%9d bytes", at50)
	t.Logf("chunks=100  streamed=%9d bytes", at100)
	t.Logf("chunks=200  streamed=%9d bytes", at200)

	for _, pair := range []struct{ small, big int }{{at50, at100}, {at100, at200}} {
		if ratio := float64(pair.big) / float64(pair.small); ratio > 2.5 {
			t.Errorf("doubling the chunks multiplied the bytes by %.2f: the stream is re-shipping the output", ratio)
		}
	}
}

// streamedChildBytes is what one connection receives for a container holding
// `children` tasks that start and finish one flush at a time — the shape of a
// plan whose steps sit under one do:/in_parallel:.
func streamedChildBytes(t *testing.T, children int) int {
	t.Helper()

	server, pipeline := testPipeline(t)
	ctx := t.Context()
	runID := fmt.Sprintf("run-children-%d", children)

	err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows := make([]store.RunEventRow, 0, 1+3*children)
	rows = append(rows, store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 0, StepName: "all", StepKind: "do", StepID: 1})

	for i := range children {
		id := int64(i + 2)
		name := fmt.Sprintf("task-%d", i)
		rows = append(rows,
			store.RunEventRow{Type: events.TypeStepStarted, StepIndex: i + 1, StepName: name, StepKind: "task", StepID: id, ParentStepID: 1},
			store.RunEventRow{Type: events.TypeStepOutput, StepIndex: i + 1, StepName: name, StepKind: "task", StepID: id, ParentStepID: 1, Text: "built the thing, then linked it against everything else\n"},
			store.RunEventRow{Type: events.TypeStepFinished, StepIndex: i + 1, StepName: name, StepKind: "task", StepID: id, ParentStepID: 1, Status: "succeeded", Hash: fmt.Sprintf("%012x", i)},
		)
	}

	appendEvents(t, pipeline.Store, runID, rows)

	err = pipeline.Store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	return len(streamOf(t, server, "/p/demo/runs/"+runID+"/events?after=1"))
}

// TestStreamBytesGrowLinearlyWithChildren is the container's version of the
// curve: a plan that puts its steps under one do: used to re-send the whole
// container, every child included, on every event under it.
func TestStreamBytesGrowLinearlyWithChildren(t *testing.T) {
	shrinkRunEventLimit(t, 5000, 1)

	at25, at50, at100 := streamedChildBytes(t, 25), streamedChildBytes(t, 50), streamedChildBytes(t, 100)
	t.Logf("children= 25  streamed=%9d bytes", at25)
	t.Logf("children= 50  streamed=%9d bytes", at50)
	t.Logf("children=100  streamed=%9d bytes", at100)

	for _, pair := range []struct{ small, big int }{{at25, at50}, {at50, at100}} {
		if ratio := float64(pair.big) / float64(pair.small); ratio > 2.5 {
			t.Errorf("doubling the children multiplied the bytes by %.2f: the stream is re-shipping the container", ratio)
		}
	}
}

// resultRun records one agent step that got `results` tool results of about
// `size` bytes each, every one carrying a marker of its own, and returns the
// page and the stream a watcher who connected after the first event gets.
func resultRun(t *testing.T, results, size int) (string, string) {
	t.Helper()

	server, pipeline := testPipeline(t)
	ctx := t.Context()
	runID := fmt.Sprintf("run-results-%d", size)

	err := pipeline.Store.StartRun(ctx, runID, "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	rows := make([]store.RunEventRow, 0, 1+results)
	rows = append(rows, store.RunEventRow{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1})

	line := strings.Repeat("x", 63) + `\n`
	for i := range results {
		rows = append(rows, store.RunEventRow{
			Type: events.TypeAgentResult, StepIndex: 0, StepName: "review", StepID: 1, Name: "read_file",
			Detail: fmt.Sprintf(`{"content":"RESULT-MARKER-%d\n%s"}`, i, strings.Repeat(line, size/len(line))),
		})
	}

	appendEvents(t, pipeline.Store, runID, rows)

	err = pipeline.Store.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/"+runID)

	return page, sseHTML(streamOf(t, server, "/p/demo/runs/"+runID+"/events?after=1"))
}

// TestRunPageBytesDoNotGrowWithResultSize is #171: every tool result's body
// was drawn up front, collapsed, so a long review's page was megabytes of
// markup nobody opened. The page and the stream now carry a summary and a URL
// per result, and a result sixteen times bigger costs them nothing.
func TestRunPageBytesDoNotGrowWithResultSize(t *testing.T) {
	shrinkRunEventLimit(t, 5000, 1)

	const results = 50

	smallPage, smallStream := resultRun(t, results, 1000)
	bigPage, bigStream := resultRun(t, results, 16000)

	for _, drawn := range []struct {
		what       string
		small, big string
	}{{"page", smallPage, bigPage}, {"stream", smallStream, bigStream}} {
		t.Logf("%s: 1 kB results=%d bytes, 16 kB results=%d bytes", drawn.what, len(drawn.small), len(drawn.big))

		if strings.Contains(drawn.big, "RESULT-MARKER-") {
			t.Errorf("the %s carries a result's body", drawn.what)
		}

		// Drawing no box at all would pass the ratio too.
		if got := len(resultSrcs(drawn.big)); got != results {
			t.Errorf("the %s points %d boxes at a body, want %d", drawn.what, got, results)
		}

		if ratio := float64(len(drawn.big)) / float64(len(drawn.small)); ratio > 1.02 {
			t.Errorf("results 16x bigger made the %s %.2fx bigger", drawn.what, ratio)
		}
	}
}
