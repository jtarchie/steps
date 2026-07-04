package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/journal"
)

func stripColors(t *testing.T) {
	t.Helper()
	saved := []*string{&cReset, &cDim, &cBold, &cBlue, &cGreen, &cYellow, &cRed, &cCyan, &cMag}
	values := make([]string, len(saved))
	for i, p := range saved {
		values[i], *p = *p, ""
	}
	t.Cleanup(func() {
		for i, p := range saved {
			*p = values[i]
		}
	})
}

func TestCondensedListenerLines(t *testing.T) {
	stripColors(t)
	var buf bytes.Buffer
	l := &condensedListener{w: &buf}

	l.RunStarted("run1", "demo", map[string]any{"article": "..."})
	l.StateEntered("draft", "agent", 1, "mock")
	l.AgentMessage("draft", "user", "should not print")
	l.ToolCalled("draft", "file.write", nil)
	l.HandlerFinished("draft", map[string]any{"score": 6.0}, "revise", journal.Usage{InputTokens: 900, OutputTokens: 300})
	l.HandlerFailed("draft", "schema_violation", errors.New("bad json"), 1)
	l.RetryScheduled("draft", "schema_violation", 2, 0)
	l.TransitionFired("draft", "critique", "", "")
	l.RunParked("run1", "escalate", "Approve?", time.Hour, &journal.ParkChoices{
		Kind: "single",
		Options: []journal.ParkOption{
			{Event: "approved", Label: "Ship it"},
			{Event: "rejected", Label: "Abort"},
		},
	})
	l.RunFinished("run1", journal.StatusDone, "done", 5, journal.Usage{InputTokens: 2000, OutputTokens: 500})

	out := buf.String()
	for _, want := range []string{
		"▶ run run1 demo (input: article)",
		"✔ draft",
		"revise",
		"1.2k tok",
		"score=6",
		"tools=1",
		"✖ draft",
		"schema_violation (attempt 1): bad json",
		"re-prompting with feedback (attempt 2)",
		"⏸ parked at escalate — Approve?",
		"1) approved  Ship it",
		"2) rejected  Abort",
		"■ done at done (5 transitions, 2.5k tokens)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	for _, dontWant := range []string{
		"should not print", // agent messages are -v territory
		"draft → critique", // transitions are implied by the state lines
	} {
		if strings.Contains(out, dontWant) {
			t.Errorf("output should not contain %q\n---\n%s", dontWant, out)
		}
	}
}

func TestHint(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"nil", nil, ""},
		{"score wins", map[string]any{"score": 8.0, "text": "long"}, "score=8"},
		{"path", map[string]any{"path": "out/x.md", "bytes": 12.0}, "out/x.md"},
		{"summary clipped", map[string]any{"summary": strings.Repeat("a", 60)}, strings.Repeat("a", 40) + "…"},
		{"scalar fallback sorted", map[string]any{"ok": true, "zz": "later"}, "ok=true"},
	}
	for _, tc := range cases {
		if got := hint(tc.in); got != tc.want {
			t.Errorf("%s: hint = %q, want %q", tc.name, got, tc.want)
		}
	}
}
