package runview

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// screen replays what Live wrote onto a grid the way a terminal would — cursor up, erase to the end, newline — and returns the rows left on it. Only the three sequences Live writes are understood, which is the point: a fourth would show up here as litter.
func screen(t *testing.T, written string) []string {
	t.Helper()

	var rows []string

	cursor := 0

	for len(written) > 0 {
		if loc := regexp.MustCompile(`^\x1b\[(\d+)A\r\x1b\[J`).FindStringSubmatchIndex(written); loc != nil {
			var up int

			for _, c := range written[loc[2]:loc[3]] {
				up = up*10 + int(c-'0')
			}

			cursor -= up
			rows = rows[:cursor]
			written = written[loc[1]:]

			continue
		}

		end := strings.IndexByte(written, '\n')
		if end < 0 {
			t.Fatalf("Live left a line unterminated: %q", written)
		}

		rows = append(rows[:cursor], sgr.ReplaceAllString(written[:end], ""))
		cursor++
		written = written[end+1:]
	}

	return rows
}

func liveAt(out *strings.Builder, clock *time.Time) *Live {
	live := NewLive(out)
	live.Width = func() int { return 60 }
	live.now = func() time.Time { return *clock }

	return live
}

// TestLiveDrawsWhatIsRunningAndScrollsWhatFinished is the region's contract: while a step runs the screen shows it with its clock and the tail of what it printed; once it finishes the region is gone from where it was and one line per step is left in scrollback.
func TestLiveDrawsWhatIsRunningAndScrollsWhatFinished(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	now := start

	var out strings.Builder

	live := liveAt(&out, &now)
	ev := func(e events.Event) {
		e.RunID, e.Job = "R", "build"
		if e.At.IsZero() {
			e.At = now
		}

		live.Event(e)
	}

	ev(events.Event{Type: events.TypeJobStarted, StepIndex: -1})
	ev(events.Event{Type: events.TypeStepStarted, StepKind: "task", StepName: "compile", StepID: 1})

	stream := live.Stream(1, false)
	for i := range 8 {
		_, _ = stream.Write([]byte("[compile] line " + string(rune('0'+i)) + "\n"))
	}

	_, _ = stream.Write([]byte("progress 10%\rprogress 90%"))

	now = start.Add(8200 * time.Millisecond)
	ev(events.Event{Type: events.TypeStepNote, StepID: 1, Status: events.NoteWarn, Text: "slow disk"})

	running := screen(t, out.String())
	want := []string{
		"warning: slow disk",
		"[+] build 8.2s  0/1 steps",
		"  ▸ task compile  8.2s",
		"    │ line 3", "    │ line 4", "    │ line 5", "    │ line 6", "    │ line 7",
		"    │ progress 90%",
	}

	if strings.Join(running, "\n") != strings.Join(want, "\n") {
		t.Errorf("while running the screen reads:\n%s\nwant:\n%s", strings.Join(running, "\n"), strings.Join(want, "\n"))
	}

	ev(events.Event{Type: events.TypeStepFinished, StepKind: "task", StepName: "compile", StepID: 1, Status: "succeeded", DurationMS: 8200})
	ev(events.Event{Type: events.TypeStepSkipped, StepKind: "task", StepName: "test", StepID: 2, Status: "skipped", Text: "when: guard was false"})
	ev(events.Event{Type: events.TypeJobFinished, StepIndex: -1, Status: "succeeded", DurationMS: 8300})
	live.Stop()

	finished := screen(t, out.String())
	want = []string{
		"warning: slow disk",
		"✓ task compile · 8.2s",
		"↷ task test skipped · when: guard was false",
		"",
		"✓ build succeeded in 8.3s · 2 steps",
	}

	if strings.Join(finished, "\n") != strings.Join(want, "\n") {
		t.Errorf("after the run the screen reads:\n%s\nwant:\n%s", strings.Join(finished, "\n"), strings.Join(want, "\n"))
	}
}

// TestLiveEndsAFailureWithTheOutputThatExplainsIt: the tail that showed a failing step's last lines is gone with the region, so the summary is followed by its whole recorded output.
func TestLiveEndsAFailureWithTheOutputThatExplainsIt(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "lint", StepID: 1},
		{Type: events.TypeStepOutput, StepKind: "task", StepName: "lint", StepID: 1, Text: "a.go:1 unused\nb.go:2 shadow"},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "lint", StepID: 1, Status: "failed", DurationMS: 300},
		{Type: events.TypeJobFinished, StepIndex: -1, Status: "failed", DurationMS: 400},
	} {
		e.RunID, e.Job, e.At = "R", "build", now
		live.Event(e)
	}

	got := strings.Join(screen(t, out.String()), "\n")
	want := strings.Join([]string{
		"✗ task lint failed · 0.3s",
		"",
		"✗ build failed in 0.4s · 1 steps",
		"",
		"── task lint ──",
		"a.go:1 unused",
		"b.go:2 shadow",
	}, "\n")

	if got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// TestLiveGivesThePromptTheTerminal: a person answering ask_user must not have the region redrawn over what they are typing.
func TestLiveGivesThePromptTheTerminal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)
	live.Event(events.Event{Type: events.TypeStepStarted, RunID: "R", Job: "build", StepKind: "agent", StepName: "ask", StepID: 1, At: now})

	prompt, release := live.Hold()
	before := out.Len()

	_, _ = prompt.Write([]byte("question 1> "))
	live.Event(events.Event{Type: events.TypeAgentCall, RunID: "R", StepID: 1, Name: "ask_user"})

	if held := out.String()[before:]; held != "question 1> " {
		t.Errorf("while held, the terminal got %q besides the prompt", held)
	}

	_, _ = out.WriteString("minor\n")

	release()

	if got := screen(t, out.String()); got[len(got)-1] != "  ▸ agent ask  0.0s  1 calls · ask_user" {
		t.Errorf("after the answer the region is back as:\n%s", strings.Join(got, "\n"))
	}
}

// TestClipNeverOverrunsTheTerminal: a row wider than the terminal wraps, and a wrapped row is two lines the next clear does not know to erase.
func TestClipNeverOverrunsTheTerminal(t *testing.T) {
	t.Parallel()

	if got := clip("▸ task compile-everything-in-the-repo", 10); got != "▸ task com" {
		t.Errorf("clip = %q", got)
	}

	if got := clip("short", 10); got != "short" {
		t.Errorf("clip = %q", got)
	}
}

// TestLiveLetsARunEndForGood: the resume hint and the usage report are said after job_finished, and a run brought back by them would sit on screen as a header nobody takes down — under which `steps test` prints its next PASS/FAIL, for the next redraw to erase.
func TestLiveLetsARunEndForGood(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "lint", StepID: 1},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "lint", StepID: 1, Status: "failed", DurationMS: 300},
		{Type: events.TypeJobFinished, StepIndex: -1, Status: "failed", DurationMS: 400},
		{Type: events.TypeStepNote, StepIndex: -1, Status: events.NoteInfo, Text: "run: R  (resume with: steps run <pipeline> --resume R)"},
	} {
		e.RunID, e.At = "R", now
		live.Event(e)
	}

	_, _ = out.WriteString("FAIL build: exit 1\n")

	live.Stop()

	got := screen(t, out.String())
	if last := got[len(got)-1]; last != "FAIL build: exit 1" {
		t.Errorf("the line printed after the run is gone; the screen reads:\n%s", strings.Join(got, "\n"))
	}

	for _, row := range got {
		if strings.HasPrefix(row, "[+]") {
			t.Errorf("a finished run was drawn again:\n%s", strings.Join(got, "\n"))
		}
	}
}

// TestLiveScrollsWhatNoRunningStepOwns: a tail is drawn only under a running step, so bytes for any other — a job-level hook, an image pull before the first step, a hook that runs after its step finished — go to scrollback, or nobody sees them.
func TestLiveScrollsWhatNoRunningStepOwns(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	_, _ = live.Stream(0, false).Write([]byte("pulling image: alpine\npartial"))
	_, _ = live.Stream(0, false).Write([]byte(" line\n"))

	live.Event(events.Event{Type: events.TypeStepStarted, RunID: "R", Job: "build", StepKind: "task", StepName: "unit", StepID: 1, At: now})
	live.Event(events.Event{Type: events.TypeStepFinished, RunID: "R", Job: "build", StepKind: "task", StepName: "unit", StepID: 1, Status: "succeeded", At: now})

	_, _ = live.Stream(1, false).Write([]byte("[notify] sent\n"))
	_, _ = live.Stream(1, false).Write([]byte("\n"))

	live.Stop()

	got := strings.Join(screen(t, out.String()), "\n")
	if want := strings.Join([]string{"pulling image: alpine", "partial line", "✓ task unit · 0.0s", "[notify] sent", ""}, "\n"); got != want {
		t.Errorf("screen:\n%s\nwant:\n%s", got, want)
	}
}

// TestTailReadsLikeATerminal: CRLF is a line ending, a bare CR starts the line over even across writes, and only the runner's own "[step] " label is taken off.
func TestTailReadsLikeATerminal(t *testing.T) {
	t.Parallel()

	var tl tail

	tl.write([]byte("one\r\ntwo\r"))
	tl.write([]byte("\n[INFO] three\n[unit] four\n10%\r"))
	tl.write([]byte("90%"))

	got := strings.Join(tl.last("unit"), "|")
	if want := "one|two|[INFO] three|four|90%"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}

	var ended tail

	ended.write([]byte("one\ntwo\n"))

	if got := strings.Join(ended.last("unit"), "|"); got != "one|two" {
		t.Errorf("a tail ending on a newline = %q, want no empty line after it", got)
	}
}

// TestLiveRegionFitsTheScreen: a region taller than the terminal cannot be moved back over, and every redraw would leave a copy of it in scrollback.
func TestLiveRegionFitsTheScreen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)
	live.Height = func() int { return 4 }

	for id := int64(1); id <= 6; id++ {
		live.Event(events.Event{Type: events.TypeStepStarted, RunID: "R", Job: "build", StepKind: "task", StepName: "t", StepID: id, At: now})
	}

	if live.drawn != 3 {
		t.Errorf("region drew %d rows on a 4-row terminal, want 3", live.drawn)
	}
}

// TestEventOfIsRowOfsInverse: a follower replays rows through EventOf into the Live that rowOf folds, so a field only one of them carries is lost on the way.
func TestEventOfIsRowOfsInverse(t *testing.T) {
	t.Parallel()

	row := store.RunEventRow{
		Seq: 7, RunID: "R", Type: events.TypeStepFinished, StepIndex: 2, StepName: "lint", StepKind: "task",
		StepID: 5, ParentStepID: 3, Status: "failed", Hash: "h", Text: "t", Name: "n", Detail: "d",
		DurationMS: 42, Worker: "w (addr)", At: time.Date(2026, 9, 29, 1, 2, 3, 4, time.UTC),
	}

	event := EventOf(row, "build")
	if event.Job != "build" {
		t.Errorf("Job = %q, want build", event.Job)
	}

	if back := rowOf(event); back != row {
		t.Errorf("rowOf(EventOf(row)) = %+v, want %+v", back, row)
	}
}

// TestLiveAsksSpendWhenAnAgentFinishes: spend is recorded when an agent step finishes, so that is the only time asking it can change the answer.
func TestLiveAsksSpendWhenAnAgentFinishes(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	var asked []string

	finished := 0
	live.Spend = func(runID string) string {
		asked = append(asked, runID)

		return fmt.Sprintf("spent after %d steps", finished)
	}

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "unit", StepID: 1},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "unit", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepKind: "agent", StepName: "review", StepID: 2},
		{Type: events.TypeStepFinished, StepKind: "agent", StepName: "review", StepID: 2, Status: "succeeded"},
		{Type: events.TypeJobFinished, StepIndex: -1, Status: "succeeded", DurationMS: 400},
	} {
		if e.Type == events.TypeStepFinished {
			finished++
		}

		e.RunID, e.Job, e.At = "R", "build", now
		live.Event(e)
	}

	if strings.Join(asked, ",") != "R" {
		t.Errorf("spend was asked for %q, want once, for R", asked)
	}

	got := screen(t, out.String())
	if want := "✓ build succeeded in 0.4s · 2 steps · spent after 2 steps"; got[len(got)-1] != want {
		t.Errorf("summary = %q, want %q", got[len(got)-1], want)
	}
}

// TestLiveKeepsDrawingTheRunsStillGoing: two jobs share the region, and the one that finished first must take only itself down.
func TestLiveKeepsDrawingTheRunsStillGoing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{RunID: "A", Job: "build", Type: events.TypeStepStarted, StepKind: "task", StepName: "unit", StepID: 1},
		{RunID: "B", Job: "deploy", Type: events.TypeStepStarted, StepKind: "task", StepName: "push", StepID: 1},
		{RunID: "A", Job: "build", Type: events.TypeStepFinished, StepKind: "task", StepName: "unit", StepID: 1, Status: "succeeded"},
		{RunID: "A", Job: "build", Type: events.TypeJobFinished, StepIndex: -1, Status: "succeeded"},
	} {
		e.At = now
		live.Event(e)
	}

	got := screen(t, out.String())
	if want := []string{"[+] deploy 0.0s  0/1 steps", "  ▸ task push  0.0s"}; strings.Join(got[len(got)-2:], "\n") != strings.Join(want, "\n") {
		t.Errorf("the run still going is not drawn:\n%s", strings.Join(got, "\n"))
	}
}

// TestLiveHeaderCountsAndRowsClip: the header counts what finished, and a running row stops a column short of the terminal's width.
func TestLiveHeaderCountsAndRowsClip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)

	for _, e := range []events.Event{
		{Type: events.TypeStepStarted, StepKind: "task", StepName: "unit", StepID: 1},
		{Type: events.TypeStepFinished, StepKind: "task", StepName: "unit", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepKind: "task", StepName: strings.Repeat("x", 80), StepID: 2},
	} {
		e.RunID, e.Job, e.At = "R", "build", now
		live.Event(e)
	}

	got := screen(t, out.String())
	want := []string{
		"✓ task unit · 0.0s",
		"[+] build 0.0s  1/2 steps",
		"  ▸ task " + strings.Repeat("x", 50),
	}

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("screen:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestLiveColoursAWarningAndNothingElseOfANote: a warning is the note a reader should not scroll past.
func TestLiveColoursAWarningAndNothingElseOfANote(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var out strings.Builder

	live := liveAt(&out, &now)
	live.Color = true

	live.Event(events.Event{Type: events.TypeStepNote, RunID: "R", StepIndex: -1, Status: events.NoteWarn, Text: "slow disk"})
	live.Event(events.Event{Type: events.TypeStepNote, RunID: "R", StepIndex: -1, Status: events.NoteInfo, Text: "fyi"})

	if want := "\x1b[33mwarning: slow disk\x1b[0m\n" + "fyi\n"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}

// TestClockTurnsToMinutesAtAMinute: "60.0s" is a reading a person has to convert.
func TestClockTurnsToMinutesAtAMinute(t *testing.T) {
	t.Parallel()

	for d, want := range map[time.Duration]string{59900 * time.Millisecond: "59.9s", time.Minute: "1m00s", 61 * time.Second: "1m01s"} {
		if got := clock(d); got != want {
			t.Errorf("clock(%s) = %q, want %q", d, got, want)
		}
	}
}
