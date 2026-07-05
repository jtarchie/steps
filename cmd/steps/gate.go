package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
)

// gateAnswer is what the human chose at a parked gate.
type gateAnswer struct {
	event string
	data  map[string]any
}

// interactive reports whether gates may prompt inline: a human on both ends
// and --no-prompt unset. Non-TTY (pipes, CI) keeps park-and-exit.
func interactive() bool {
	return !flagNoPrompt &&
		isatty.IsTerminal(os.Stdin.Fd()) &&
		isatty.IsTerminal(os.Stderr.Fd())
}

// driveGates keeps an interactive session going across gates: while the run
// is parked and we can prompt, ask and resume in-process. A later gate may
// park again — hence the loop. Leaving the answer empty keeps the run parked
// (the park block above already printed the resume command).
func driveGates(ctx context.Context, eng *engine.Engine, m *machine.Machine, res *engine.Result) (*engine.Result, error) {
	in := bufio.NewReader(os.Stdin)
	for res.Status == journal.StatusParked && res.State.Parked != nil && interactive() {
		// The RunParked narration just printed the prompt and options.
		ans := promptGate(in, res.State.Parked, false)
		if ans == nil {
			return res, nil // leave parked
		}
		next, err := eng.Resume(ctx, m, res.RunID, ans.event, ans.data)
		if err != nil {
			// A free-form event outside the gate's alphabet is the one
			// answer we cannot pre-validate — re-prompt instead of dying.
			if strings.Contains(err.Error(), "no route for event") {
				fmt.Fprintf(os.Stderr, "%s✖ %v%s\n", cRed, err, cReset)
				continue
			}
			return nil, err
		}
		res = next
	}
	return res, nil
}

// promptGate collects one gate answer from stdin. showGate reprints the
// prompt and options first (used by `steps resume` where no park narration
// preceded). Returns nil when the user leaves the run parked (empty / EOF).
func promptGate(in *bufio.Reader, p *journal.ParkInfo, showGate bool) *gateAnswer {
	if showGate {
		fmt.Fprintf(os.Stderr, "%s⏸ %s%s — %s\n", cYellow, p.State, cReset, p.Prompt)
		printParkChoices(func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}, p.Choices)
	}
	for {
		fmt.Fprintf(os.Stderr, "%s%s%s ", cBold, selectionPrompt(p.Choices), cReset)
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(os.Stderr)
			return nil // EOF: leave parked
		}
		ans, perr := parseGateSelection(p.Choices, strings.TrimSpace(line))
		if perr != nil {
			fmt.Fprintf(os.Stderr, "%s✖ %v%s\n", cRed, perr, cReset)
			continue
		}
		if ans == nil {
			return nil // empty: leave parked
		}
		fmt.Fprintf(os.Stderr, "%snote (optional, enter to skip):%s ", cDim, cReset)
		note, _ := in.ReadString('\n')
		if note = strings.TrimSpace(note); note != "" {
			if ans.data == nil {
				ans.data = map[string]any{}
			}
			ans.data["note"] = note
		}
		return ans
	}
}

func selectionPrompt(c *journal.ParkChoices) string {
	switch {
	case c == nil || len(c.Options) == 0:
		return "event (enter to leave parked):"
	case c.Kind == "multi":
		bounds := ""
		if c.Min > 0 || c.Max > 0 {
			bounds = fmt.Sprintf(" [min %d", c.Min)
			if c.Max > 0 {
				bounds += fmt.Sprintf(", max %d", c.Max)
			}
			bounds += "]"
		}
		return fmt.Sprintf("choose numbers (comma-separated, e.g. 1,3) or 'all'%s (enter to leave parked):", bounds)
	default:
		return fmt.Sprintf("choose 1-%d or an event name (enter to leave parked):", len(c.Options))
	}
}

// parseGateSelection maps one input line to a gate answer. Pure — the
// testable core of the prompt. Empty input means "leave parked" (nil, nil).
func parseGateSelection(c *journal.ParkChoices, line string) (*gateAnswer, error) {
	if line == "" {
		return nil, nil //nolint:nilnil // (nil, nil) is "leave parked", a valid non-error outcome; see the doc comment above
	}
	// Free-form event: no options to choose from, or a typed event name.
	if c == nil || len(c.Options) == 0 {
		return &gateAnswer{event: line}, nil
	}

	if c.Kind == "multi" {
		var selected []any
		if line == "all" {
			for _, opt := range c.Options {
				selected = append(selected, opt.Value)
			}
		} else {
			for _, part := range strings.Split(line, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				n, err := strconv.Atoi(part)
				if err != nil || n < 1 || n > len(c.Options) {
					return nil, fmt.Errorf("%q is not an option number between 1 and %d", part, len(c.Options))
				}
				selected = append(selected, c.Options[n-1].Value)
			}
		}
		if c.Min > 0 && len(selected) < c.Min {
			return nil, fmt.Errorf("select at least %d option(s)", c.Min)
		}
		if c.Max > 0 && len(selected) > c.Max {
			return nil, fmt.Errorf("select at most %d option(s)", c.Max)
		}
		return &gateAnswer{event: c.Event, data: map[string]any{"selected": selected}}, nil
	}

	// Single: a number picks an option; anything else is a free-form event
	// name (authored choices may cover only part of the gate's alphabet).
	if n, err := strconv.Atoi(line); err == nil {
		if n < 1 || n > len(c.Options) {
			return nil, fmt.Errorf("choose a number between 1 and %d", len(c.Options))
		}
		return &gateAnswer{event: c.Options[n-1].Event}, nil
	}
	return &gateAnswer{event: line}, nil
}

// resumeInteractive handles `steps resume <id>` with no --event on a TTY:
// present the parked gate and collect the answer. Returns false when the
// run is not parked (crash-resume) or prompting is off — callers fall back
// to the plain Resume path.
func resumeInteractive(ctx context.Context, eng *engine.Engine, store journal.Store, m *machine.Machine, runID string) (*engine.Result, bool, error) {
	events, err := store.Events(ctx, runID)
	if err != nil {
		return nil, false, err
	}
	rs := journal.Fold(events)
	p := rs.Parked
	if p == nil || rs.Finished || !interactive() || p.Expired(time.Now()) {
		return nil, false, nil
	}
	ans := promptGate(bufio.NewReader(os.Stdin), p, true)
	if ans == nil {
		return nil, true, nil // answered "leave parked": done, no result
	}
	res, err := eng.Resume(ctx, m, runID, ans.event, ans.data)
	if err != nil {
		return nil, true, err
	}
	res, err = driveGates(ctx, eng, m, res)
	return res, true, err
}
