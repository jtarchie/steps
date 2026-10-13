package runview

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
)

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
		"stray\x1b[\nERROR kept":                       "stray\nERROR kept",
		"osc\x1b]0;unterminated\nFAIL kept":            "osc\nFAIL kept",
		"osc\x1b]0;ends on\x1b":                        "osc",
	} {
		if got := Scrub(input); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", input, got, want)
		}
	}

	event := ScrubEvent(events.Event{
		Text: "\x1b]0;t\x07x", Name: "\x1b[1mn", Detail: "d\x00", StepName: "s\x1b[K", Worker: "w\x07",
		Job: "j\x1b]52;c;ZXZpbA==\x07", StepKind: "k\x1b[2J", Status: "f\x07",
	})
	want := events.Event{Text: "x", Name: "n", Detail: "d", StepName: "s", Worker: "w", Job: "j", StepKind: "k", Status: "f"}

	if event != want {
		t.Errorf("ScrubEvent left escapes in a field: %+v", event)
	}
}

// TestPlainAndLiveDropAnOSC: an event's text can come from an untrusted PR, and an OSC in it rewrites the viewer's clipboard, title or links in whichever terminal renders it.
func TestPlainAndLiveDropAnOSC(t *testing.T) {
	t.Parallel()

	const osc = "\x1b]0;pwned\x07"

	var plain strings.Builder

	Plain(&plain)(events.Event{Type: events.TypeStepNote, Status: events.NoteInfo, Text: "note" + osc})
	Plain(&plain)(events.Event{Type: events.TypeStepStarted, StepKind: "task", StepName: "s" + osc})

	if got := plain.String(); got != "note\ntask: s\n" {
		t.Errorf("Plain wrote %q", got)
	}

	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

	var live strings.Builder

	view := liveAt(&live, &now)
	view.Event(events.Event{Type: events.TypeStepNote, RunID: "R", StepIndex: -1, Status: events.NoteInfo, Text: "note" + osc})
	view.Event(events.Event{Type: events.TypeJobStarted, RunID: "R", Job: "build" + osc, StepIndex: -1, At: now})
	view.Event(events.Event{Type: events.TypeStepStarted, RunID: "R", StepKind: "task", StepName: "s" + osc, StepID: 1, At: now})
	_, _ = view.Stream(1, false).Write([]byte("tail" + osc + "\x1b]8;;http://evil\x1b\\link\n"))
	view.Event(events.Event{Type: events.TypeStepNote, RunID: "R", StepIndex: -1, Status: events.NoteInfo, Text: "redraw"})
	view.Event(events.Event{Type: events.TypeStepFinished, RunID: "R", StepKind: "task", StepName: "s" + osc, StepID: 1, Status: "succeeded"})
	view.Stop()

	if strings.Contains(live.String(), "\x1b]") || strings.Contains(live.String(), "pwned") {
		t.Errorf("Live wrote an OSC: %q", live.String())
	}

	if !strings.Contains(live.String(), "taillink") {
		t.Errorf("Live dropped the tail's text with its escapes: %q", live.String())
	}
}
