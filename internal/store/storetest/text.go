package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// hostile is text a task's output or a model's answer can carry and that a
// database may refuse to store as text: a NUL byte and an invalid UTF-8
// sequence, between two markers a reader must still see.
const hostile = "before\x00middle\xff\xfeafter"

// TestHostileTextIsRecorded: what steps stores is mostly text it did not
// write, and the event sink only WARNS on a failed insert. A database that
// refuses a byte therefore loses the whole row in silence — a run that
// finished with a hole in its story. Recorded, and readable, is the contract;
// the bytes it comes back as are the driver's business.
func (s suite) TestHostileTextIsRecorded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "build", "run-1")

	err := st.AppendRunEvent(ctx, store.RunEventRow{
		RunID: "run-1", Type: "step_note", StepName: hostile, Text: hostile, Detail: hostile, At: time.Now(),
	})
	if err != nil {
		t.Fatalf("AppendRunEvent: %v", err)
	}

	events := mustRunEvents(ctx, t, st, "run-1", 0, 0)
	if len(events) != 1 {
		t.Fatalf("%d events recorded, want 1", len(events))
	}

	assertReadable(t, "event text", events[0].Text)

	mustRecordNode(t, st, "build", hashOf(1))

	err = st.SaveNodeTranscript(ctx, hashOf(1), `[{"type":"text","text":"`+hostile+`"}]`)
	if err != nil {
		t.Fatalf("SaveNodeTranscript: %v", err)
	}

	transcript, found, err := st.NodeTranscript(ctx, hashOf(1))
	if err != nil || !found {
		t.Fatalf("NodeTranscript: found=%v err=%v", found, err)
	}

	assertReadable(t, "transcript", transcript)

	mustEnqueueJob(t, st, "build", "r")
	id, _ := mustClaimJob(t, st, "build")

	err = st.CompleteJob(ctx, id, "failed", errors.New(hostile))
	if err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	rows, err := st.ListTriggerQueue(ctx, 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListTriggerQueue: %d rows, %v", len(rows), err)
	}

	assertReadable(t, "queue error", rows[0].Error)
}

func assertReadable(t *testing.T, what, got string) {
	t.Helper()

	if !strings.Contains(got, "before") {
		t.Errorf("%s = %q, want the text before the hostile bytes", what, got)
	}

	if !strings.Contains(got, "after") {
		t.Errorf("%s = %q, want the text after the hostile bytes", what, got)
	}
}
