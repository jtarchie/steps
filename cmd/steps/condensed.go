package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/steps/journal"
)

// condensedListener is the default narration: one line per state execution
// (state, event, tokens, duration, a small result hint), plus failures,
// retries, and the run header/footer. -v switches to prettyListener.
type condensedListener struct {
	w io.Writer // defaults to os.Stderr; injected in tests

	// The engine drives states sequentially, so one slot tracks the
	// execution in flight.
	state   string
	started time.Time
	tools   int
	memo    bool
}

func (l *condensedListener) p(format string, args ...any) {
	w := l.w
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format+"\n", args...)
}

func (l *condensedListener) RunStarted(runID, machineName string, input map[string]any) {
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	l.p("%s▶ run %s%s%s %s %s(input: %s)%s", cBold, cCyan, runID, cReset+cBold, machineName, cDim, strings.Join(keys, ", "), cReset)
}

func (l *condensedListener) StateEntered(state, kind string, visit int, model string) {
	l.state, l.started, l.tools, l.memo = state, time.Now(), 0, false
}

func (l *condensedListener) ForEachItem(string, int, int, any) {}

func (l *condensedListener) MemoHit(string) { l.memo = true }

func (l *condensedListener) AgentMessage(string, string, string) {}

func (l *condensedListener) ToolCalled(state, tool string, args map[string]any) { l.tools++ }

func (l *condensedListener) ToolResult(string, string, map[string]any) {}

func (l *condensedListener) ToolRejected(state, tool, reason, mode string) {
	l.p("  %s⛔ %s rejected (%s):%s %s", cRed, tool, mode, cReset, clipLine(reason, 140))
}

func (l *condensedListener) HandlerFinished(state string, output map[string]any, event string, usage journal.Usage) {
	tok := ""
	if usage.Total() > 0 {
		tok = formatCount(usage.Total()) + " tok"
	}
	dur := ""
	if l.state == state && !l.started.IsZero() {
		dur = time.Since(l.started).Round(100 * time.Millisecond).String()
	}
	extras := hint(output)
	if l.memo {
		extras = joinSpace(extras, "⚡memo")
	}
	if l.tools > 0 {
		extras = joinSpace(extras, fmt.Sprintf("tools=%d", l.tools))
	}
	l.p("%s✔%s %-16s %s%-10s%s %8s %7s   %s%s%s",
		cGreen, cReset, state, cYellow, event, cReset, tok, dur, cDim, extras, cReset)
}

func (l *condensedListener) HandlerFailed(state, class string, err error, attempt int) {
	at := ""
	if attempt > 0 {
		at = fmt.Sprintf(" (attempt %d)", attempt)
	}
	l.p("%s✖%s %-16s %s%s%s%s: %s", cRed, cReset, state, cRed, class, at, cReset, clipLine(err.Error(), 140))
}

func (l *condensedListener) RetryScheduled(state, class string, attempt int, delay time.Duration) {
	in := ""
	if delay > 0 {
		in = fmt.Sprintf(" in %s", delay.Round(time.Millisecond))
	}
	what := "retrying"
	if class == "schema_violation" || class == "guard_rejected" {
		what = "re-prompting with feedback"
	}
	l.p("%s↻%s %-16s %s%s%s (attempt %d)%s", cYellow, cReset, state, cYellow, what, in, attempt, cReset)
}

func (l *condensedListener) TransitionFired(from, to, on, when string) {}

func (l *condensedListener) RunParked(runID, state, prompt string, timeout time.Duration, choices *journal.ParkChoices) {
	l.p("%s⏸ parked%s at %s%s%s — %s", cYellow, cReset, cBold, state, cReset, clipLine(prompt, 140))
	printParkChoices(l.p, choices)
	if timeout > 0 {
		l.p("  %sgate expires in %s%s", cDim, timeout, cReset)
	}
	l.p("  %sresume with: steps resume %s --event <event> [--data '{...}']%s", cDim, runID, cReset)
}

func (l *condensedListener) RunResumed(runID, event string) {
	l.p("%s⏵ resumed%s %s %swith event %s%s", cGreen, cReset, runID, cDim, event, cReset)
}

func (l *condensedListener) RunFinished(runID, status, terminal string, transitions int, usage journal.Usage) {
	color := cGreen
	if status == journal.StatusFailed {
		color = cRed
	}
	cost := ""
	if usage.Cost > 0 {
		cost = fmt.Sprintf(", $%.4f", usage.Cost)
	}
	l.p("%s■ %s%s at %s %s(%d transitions, %s tokens%s)%s",
		color, status, cReset, terminal, cDim, transitions, formatCount(usage.Total()), cost, cReset)
}

func (l *condensedListener) Warn(msg string, args ...any) {
	extra := ""
	for i := 0; i+1 < len(args); i += 2 {
		extra += fmt.Sprintf(" %v=%v", args[i], args[i+1])
	}
	l.p("  %s⚠ %s%s%s", cYellow, msg, extra, cReset)
}

// clipLine renders text as one loggable line of at most n runes.
func clipLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func joinSpace(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}

// hint distills a state's output to the one detail worth a glance: a score,
// a path, a count, or the leading text.
func hint(output map[string]any) string {
	if len(output) == 0 {
		return ""
	}
	if v, ok := output["score"]; ok {
		return fmt.Sprintf("score=%v", v)
	}
	if v, ok := output["path"].(string); ok {
		return v
	}
	if v, ok := output["count"]; ok {
		return fmt.Sprintf("count=%v", v)
	}
	for _, k := range []string{"summary", "text"} {
		if v, ok := output[k].(string); ok {
			return clipLine(v, 40)
		}
	}
	keys := make([]string, 0, len(output))
	for k := range output {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := output[k].(type) {
		case string:
			return k + "=" + clipLine(v, 40)
		case bool, int, int64, float64:
			return fmt.Sprintf("%s=%v", k, v)
		}
	}
	return ""
}
