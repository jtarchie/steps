package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jtarchie/steps/journal"
)

// ANSI styles, suppressed under NO_COLOR.
var (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cBlue   = "\033[34m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cCyan   = "\033[36m"
	cMag    = "\033[35m"
)

func init() {
	if os.Getenv("NO_COLOR") != "" {
		cReset, cDim, cBold, cBlue, cGreen, cYellow, cRed, cCyan, cMag = "", "", "", "", "", "", "", "", ""
	}
}

// prettyListener narrates the run to stderr, human-readable first.
type prettyListener struct {
	verbose bool
}

func (l *prettyListener) p(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// clip renders text as a single loggable line unless verbose.
func (l *prettyListener) clip(s string) string {
	s = strings.TrimSpace(s)
	if l.verbose {
		return s
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

func compactJSON(m map[string]any) string {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Sprintf("%v", m)
	}
	return string(raw)
}

func (l *prettyListener) RunStarted(runID, machineName string, input map[string]any) {
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	l.p("%s▶ run %s%s%s %s %s(input: %s)%s", cBold, cCyan, runID, cReset+cBold, machineName, cDim, strings.Join(keys, ", "), cReset)
}

func (l *prettyListener) StateEntered(state, kind string, visit int, model string) {
	detail := kind
	if model != "" {
		detail = fmt.Sprintf("%s %s", kind, model)
	}
	suffix := ""
	if visit > 1 {
		suffix = fmt.Sprintf(" %s(visit %d)%s", cYellow, visit, cReset)
	}
	l.p("%s● %s%s %s— %s%s%s", cBlue, cBold, state, cReset+cDim, detail, cReset, suffix)
}

func (l *prettyListener) AgentMessage(state, role, text string) {
	arrow, color := "→", cDim
	if role == "model" {
		arrow, color = "←", cMag
	}
	l.p("  %s%s %s:%s %s", color, arrow, role, cReset, l.clip(text))
}

func (l *prettyListener) ToolCalled(state, tool string, args map[string]any) {
	l.p("  %s⚙ %s%s %s", cCyan, tool, cReset, l.clip(compactJSON(args)))
}

func (l *prettyListener) ToolResult(state, tool string, result map[string]any) {
	l.p("  %s⚙ %s ⇒%s %s", cDim, tool, cReset, l.clip(compactJSON(result)))
}

func (l *prettyListener) ToolRejected(state, tool, reason, mode string) {
	l.p("  %s⛔ %s rejected (%s):%s %s", cRed, tool, mode, cReset, l.clip(reason))
}

func (l *prettyListener) HandlerFinished(state string, output map[string]any, event string, usage journal.Usage) {
	ev := ""
	if event != "" {
		ev = fmt.Sprintf(" %sevent=%s%s", cYellow, event, cReset)
	}
	tok := ""
	if usage.Total() > 0 {
		tok = fmt.Sprintf(" %s%s tokens%s", cDim, formatCount(usage.Total()), cReset)
	}
	l.p("  %s✔%s %s%s%s", cGreen, cReset, l.clip(compactJSON(output)), ev, tok)
}

func (l *prettyListener) HandlerFailed(state, class string, err error, attempt int) {
	at := ""
	if attempt > 0 {
		at = fmt.Sprintf(" (attempt %d)", attempt)
	}
	l.p("  %s✖ %s%s:%s %s", cRed, class, at, cReset, l.clip(err.Error()))
}

func (l *prettyListener) RetryScheduled(state, class string, attempt int, delay time.Duration) {
	in := ""
	if delay > 0 {
		in = fmt.Sprintf(" in %s", delay.Round(time.Millisecond))
	}
	what := "retrying"
	if class == "schema_violation" || class == "guard_rejected" {
		what = "re-prompting with feedback"
	}
	l.p("  %s↻ %s%s (attempt %d)%s", cYellow, what, in, attempt, cReset)
}

func (l *prettyListener) TransitionFired(from, to, on, when string) {
	cond := ""
	if on != "" {
		cond = fmt.Sprintf(" %son: %s%s", cDim, on, cReset)
	}
	if when != "" {
		cond += fmt.Sprintf(" %swhen: %s%s", cDim, when, cReset)
	}
	l.p("%s→ %s → %s%s%s", cBold, from, to, cReset, cond)
}

func (l *prettyListener) RunParked(runID, state, prompt string, timeout time.Duration) {
	l.p("%s⏸ parked%s at %s%s%s — %s", cYellow, cReset, cBold, state, cReset, l.clip(prompt))
	if timeout > 0 {
		l.p("  %sgate expires in %s%s", cDim, timeout, cReset)
	}
	l.p("  %sresume with: steps resume %s --event <event> [--data '{...}']%s", cDim, runID, cReset)
}

func (l *prettyListener) RunResumed(runID, event string) {
	l.p("%s⏵ resumed%s %s %swith event %s%s", cGreen, cReset, runID, cDim, event, cReset)
}

func (l *prettyListener) RunFinished(runID, status, terminal string, transitions int, usage journal.Usage) {
	icon, color := "■", cGreen
	if status == journal.StatusFailed {
		icon, color = "■", cRed
	}
	cost := ""
	if usage.Cost > 0 {
		cost = fmt.Sprintf(", $%.4f", usage.Cost)
	}
	l.p("%s%s %s%s at %s %s(%d transitions, %s tokens%s)%s",
		color, icon, status, cReset, terminal, cDim, transitions, formatCount(usage.Total()), cost, cReset)
}

func (l *prettyListener) Warn(msg string, args ...any) {
	var b strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	l.p("  %s⚠ %s%s%s", cYellow, msg, b.String(), cReset)
}

func formatCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
