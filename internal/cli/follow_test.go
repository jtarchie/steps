package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// fastFollow shrinks followRun's seams for one test. Tests using it are not parallel: the seams are package vars.
func fastFollow(t *testing.T, settle, quiet time.Duration) {
	t.Helper()

	poll, batch, oldSettle, oldQuiet := followPoll, followBatch, followSettle, followQuiet
	followPoll, followBatch, followSettle, followQuiet = 5*time.Millisecond, 2, settle, quiet

	t.Cleanup(func() { followPoll, followBatch, followSettle, followQuiet = poll, batch, oldSettle, oldQuiet })
}

type recorder struct {
	mu   sync.Mutex
	seen []events.Event
}

func (r *recorder) render(event events.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seen = append(r.seen, event)
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	types := make([]string, 0, len(r.seen))
	for _, event := range r.seen {
		types = append(types, event.Type)
	}

	return types
}

func (r *recorder) waitFor(t *testing.T, n int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for len(r.types()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("rendered %v, waited for %d events", r.types(), n)
		}

		time.Sleep(time.Millisecond)
	}
}

func followStoreFor(t *testing.T) *sqlite.Store {
	t.Helper()

	st, err := sqlite.OpenStore(filepath.Join(t.TempDir(), "state.db"), "app")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	err = st.StartRun(context.Background(), "R", "build", "", "")
	if err != nil {
		t.Fatal(err)
	}

	return st
}

func appendEvents(t *testing.T, st store.Events, types ...string) {
	t.Helper()

	for _, kind := range types {
		row := store.RunEventRow{RunID: "R", Type: kind, StepIndex: -1, At: time.Now()}
		if kind == events.TypeStepNote {
			row.Text = "resume with --resume R"
		}

		if kind == events.TypeJobFinished {
			row.Status = "succeeded"
		}

		err := st.AppendRunEvent(context.Background(), row)
		if err != nil {
			t.Fatal(err)
		}
	}
}

type followed struct {
	run store.RunRow
	err error
}

func startFollow(ctx context.Context, t *testing.T, st followStore, rec *recorder) <-chan followed {
	t.Helper()

	run, _, err := st.FindRunRow(ctx, "R")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan followed, 1)

	go func() {
		last, err := followRun(ctx, st, run, rec.render)
		done <- followed{last, err}
	}()

	return done
}

func await(t *testing.T, done <-chan followed) followed {
	t.Helper()

	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("followRun never returned")

		return followed{}
	}
}

// trailingNote lands the note the moment the drain that carried job_finished returns, which is the latest the one empty poll after it is there to catch.
type trailingNote struct {
	followStore

	t    *testing.T
	once sync.Once
}

func (n *trailingNote) RunEvents(ctx context.Context, runID string, after int64, limit int) ([]store.RunEventRow, error) {
	rows, err := n.followStore.RunEvents(ctx, runID, after, limit)

	for _, row := range rows {
		if row.Type == events.TypeJobFinished {
			n.once.Do(func() {
				note := store.RunEventRow{RunID: runID, Type: events.TypeStepNote, StepIndex: -1, Text: "resume with --resume R", At: time.Now()}
				err := n.AppendRunEvent(ctx, note)
				if err != nil {
					n.t.Error(err)
				}
			})
		}
	}

	return rows, err //nolint:wrapcheck // a pass-through
}

// TestFollowRunSeesTheRunOutIncludingItsTrailingNotes is the live seam: a run another process is executing, in job.go's real order — FinishRun before job_finished, a note after it.
func TestFollowRunSeesTheRunOutIncludingItsTrailingNotes(t *testing.T) {
	fastFollow(t, time.Hour, time.Hour)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted, events.TypeStepStarted)

	rec := &recorder{}
	done := startFollow(t.Context(), t, &trailingNote{followStore: st, t: t}, rec)
	rec.waitFor(t, 2)

	appendEvents(t, st, events.TypeStepFinished)

	err := st.FinishRun(context.Background(), "R", "succeeded")
	if err != nil {
		t.Fatal(err)
	}

	// The sink lagging: polls that see a final row and no job_finished yet.
	time.Sleep(30 * time.Millisecond)

	appendEvents(t, st, events.TypeJobFinished)

	result := await(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}

	want := "job_started step_started step_finished job_finished step_note"
	if got := strings.Join(rec.types(), " "); got != want {
		t.Errorf("rendered %q, want %q", got, want)
	}

	if result.run.Status != "succeeded" {
		t.Errorf("returned status %q, want succeeded", result.run.Status)
	}

	for _, event := range rec.seen {
		if event.Job != "build" {
			t.Errorf("%s rendered without its job: %+v", event.Type, event)
		}
	}
}

// TestFollowRunSynthesizesADroppedJobFinished: the sink drops on a full buffer, and without job_finished the live view keeps its region up forever.
func TestFollowRunSynthesizesADroppedJobFinished(t *testing.T) {
	fastFollow(t, 50*time.Millisecond, time.Hour)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted)

	err := st.FinishRun(context.Background(), "R", "failed")
	if err != nil {
		t.Fatal(err)
	}

	rec := &recorder{}

	result := await(t, startFollow(t.Context(), t, st, rec))
	if result.err != nil {
		t.Fatal(result.err)
	}

	types := rec.types()
	if len(types) != 2 || types[1] != events.TypeJobFinished {
		t.Fatalf("rendered %v, want a synthesized job_finished last", types)
	}

	if last := rec.seen[1]; last.Status != "failed" || last.RunID != "R" || last.Job != "build" {
		t.Errorf("synthesized %+v, want the run row's status, id and job", last)
	}
}

// TestFollowRunWaitsOutAResume: a resume reuses the run id, so the first attempt's job_finished is not the end.
func TestFollowRunWaitsOutAResume(t *testing.T) {
	fastFollow(t, time.Hour, time.Hour)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted, events.TypeJobFinished, events.TypeJobStarted, events.TypeStepStarted)

	rec := &recorder{}
	done := startFollow(t.Context(), t, st, rec)
	rec.waitFor(t, 4)

	// Several polls with the run still running after the first attempt finished.
	time.Sleep(50 * time.Millisecond)

	select {
	case result := <-done:
		t.Fatalf("stopped after the first attempt: %+v", result)
	default:
	}

	err := st.FinishRun(context.Background(), "R", "succeeded")
	if err != nil {
		t.Fatal(err)
	}

	// Polls that see the final row before the second job_finished lands: the first attempt's must not count.
	time.Sleep(30 * time.Millisecond)

	appendEvents(t, st, events.TypeJobFinished)

	result := await(t, done)
	if result.err != nil {
		t.Fatal(result.err)
	}

	if got := len(rec.types()); got != 5 {
		t.Errorf("rendered %v, want both attempts", rec.types())
	}
}

// reapingStore loses the run after its first read, the way retention or `pipeline destroy` takes it mid-follow.
type reapingStore struct {
	followStore

	mu    sync.Mutex
	reads int
}

func (r *reapingStore) FindRunRow(ctx context.Context, id string) (store.RunRow, bool, error) {
	r.mu.Lock()
	r.reads++
	reads := r.reads
	r.mu.Unlock()

	if reads > 2 {
		return store.RunRow{}, false, nil
	}

	return r.followStore.FindRunRow(ctx, id) //nolint:wrapcheck // a pass-through
}

func TestFollowRunNoticesARunReapedFromUnderIt(t *testing.T) {
	fastFollow(t, time.Hour, time.Hour)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted)

	result := await(t, startFollow(t.Context(), t, &reapingStore{followStore: st}, &recorder{}))
	if result.err == nil || !strings.Contains(result.err.Error(), "is gone") {
		t.Fatalf("err = %v, want the run named gone", result.err)
	}
}

// brokenStore fails every read, the way a replaced or locked state file does.
type brokenStore struct{ followStore }

func (brokenStore) FindRunRow(context.Context, string) (store.RunRow, bool, error) {
	return store.RunRow{}, false, errors.New("database disk image is malformed")
}

// TestFollowRunReportsAStoreFailure: a fault is returned, not retried into a hang, and not dressed as a detach.
func TestFollowRunReportsAStoreFailure(t *testing.T) {
	fastFollow(t, time.Hour, time.Hour)

	st := followStoreFor(t)

	run, _, err := st.FindRunRow(t.Context(), "R")
	if err != nil {
		t.Fatal(err)
	}

	_, err = followRun(t.Context(), brokenStore{st}, run, (&recorder{}).render)
	if err == nil || errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "malformed") {
		t.Errorf("err = %v, want the store's failure", err)
	}
}

// TestFollowRunWarnsOnceAboutAnOrphan: a run left `running` by a process that died never finishes, and nothing else would say so.
func TestFollowRunWarnsOnceAboutAnOrphan(t *testing.T) {
	fastFollow(t, time.Hour, 30*time.Millisecond)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	rec := &recorder{}
	done := startFollow(ctx, t, st, rec)

	rec.waitFor(t, 2)
	time.Sleep(100 * time.Millisecond)

	if got := strings.Join(rec.types(), " "); got != "job_started step_note" {
		t.Fatalf("rendered %q, want one warning after the silence", got)
	}

	if note := rec.seen[1]; note.Status != events.NoteWarn || !strings.Contains(note.Text, "Ctrl-C detaches") {
		t.Errorf("warning = %+v", note)
	}

	appendEvents(t, st, events.TypeStepStarted)
	rec.waitFor(t, 4)

	if got := strings.Join(rec.types(), " "); got != "job_started step_note step_started step_note" {
		t.Errorf("rendered %q, want the warning re-armed by new events", got)
	}

	cancel()

	if result := await(t, done); !errors.Is(result.err, context.Canceled) {
		t.Errorf("err = %v, want a detach", result.err)
	}
}

func TestFollowRunDetachesOnCancel(t *testing.T) {
	fastFollow(t, time.Hour, time.Hour)

	st := followStoreFor(t)
	appendEvents(t, st, events.TypeJobStarted)

	ctx, cancel := context.WithCancel(t.Context())
	rec := &recorder{}
	done := startFollow(ctx, t, st, rec)
	rec.waitFor(t, 1)
	cancel()

	result := await(t, done)
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", result.err)
	}

	if result.run.ID != "R" {
		t.Errorf("returned run %+v, want R for the detach line", result.run)
	}
}

// TestScrubLeavesOnlyText: a followed run prints bytes somebody else's input chose, onto a viewer's terminal.
func TestScrubLeavesOnlyText(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"a\x1b]52;c;ZXZpbA==\x07b":                     "ab",
		"a\x1b]0;pwned\x1b\\b":                         "ab",
		"\x1b]8;;http://evil\x1b\\click\x1b]8;;\x1b\\": "click",
		"\x1b[31mred\x1b[0m":                           "red",
		"one\rtwo\r\nthree":                            "one\ntwo\nthree",
		"nul\x00bell\x07c1\x9b31mdel\x7f":              "nulbellc131mdel",
		"c1rune\u009b31m":                              "c1rune31m",
		"tab\tkept\x1bcx":                              "tab\tkeptx",
		"ünïcode ✓":                                    "ünïcode ✓",
		"trail\x1b":                                    "trail",
		"a\x1b]0;never ends":                           "a",
		"b\x1b[31":                                     "b",
	} {
		if got := scrubText(input); got != want {
			t.Errorf("scrubText(%q) = %q, want %q", input, got, want)
		}
	}

	event := scrub(events.Event{Text: "\x1b]0;t\x07x", Name: "\x1b[1mn", Detail: "d\x00", StepName: "s\x1b[K", Worker: "w\x07"})
	if event.Text != "x" || event.Name != "n" || event.Detail != "d" || event.StepName != "s" || event.Worker != "w" {
		t.Errorf("scrub left escapes in a field: %+v", event)
	}
}
