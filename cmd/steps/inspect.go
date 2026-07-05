package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jtarchie/steps/journal"
)

// cmdInspect renders a run's journal as the review you'd otherwise write
// SQL for: per-state token usage, failures, routing, and (with --messages)
// the recorded conversations.
func cmdInspect() *cobra.Command {
	var showMessages bool
	c := &cobra.Command{
		Use:   "inspect <run-id>",
		Short: "Per-state usage, failures, and routing for a run; --messages dumps conversations",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInspect(args[0], showMessages)
		},
	}
	c.Flags().BoolVar(&showMessages, "messages", false, "dump each state's recorded conversation")
	return c
}

func runInspect(runID string, showMessages bool) error {
	store, err := journal.OpenSQLite(flagDB)
	if err != nil {
		return err
	}
	defer store.Close()

	run, err := store.GetRun(context.Background(), runID)
	if err != nil {
		return err
	}
	events, err := store.Events(context.Background(), runID)
	if err != nil {
		return err
	}
	rs := journal.Fold(events)

	printInspectHeader(run, rs)
	rows, failures := collectInspectRows(events)
	printInspectRows(rows)
	printInspectFailures(failures)
	printInspectRouting(events)
	printInspectParked(rs.Parked)
	if showMessages {
		printInspectConversations(rs)
	}
	return nil
}

func printInspectHeader(run *journal.Run, rs *journal.RunState) {
	fmt.Printf("%srun %s%s  %s  %s(%s, hash %s)%s\n",
		cBold, run.ID, cReset, run.Machine, cDim, run.Status, run.Hash[:12], cReset)
	fmt.Printf("state: %s   transitions: %d   tokens: %d in / %d out\n\n",
		rs.Current, rs.Transitions, rs.Usage.InputTokens, rs.Usage.OutputTokens)
}

// inspectRow is one state's execution: visit number, token usage, and the
// event it produced.
type inspectRow struct {
	state   string
	visit   int
	in, out int
	event   string
}

// collectInspectRows walks the journal in order into per-state execution
// rows and a state -> failure-class -> count tally.
func collectInspectRows(events []*journal.Event) (rows []inspectRow, failures map[string]map[string]int) {
	visitSeen := map[string]int{}
	failures = map[string]map[string]int{}
	for _, ev := range events {
		//exhaustive:ignore // this view only cares about per-state rows and failures; other event types carry nothing it renders
		switch ev.Type {
		case journal.HandlerFinished:
			var d struct {
				State string        `json:"state"`
				Event string        `json:"event"`
				Usage journal.Usage `json:"usage"`
				Memo  bool          `json:"memo"`
			}
			if journal.DecodeData(ev, &d) == nil {
				visitSeen[d.State]++
				event := d.Event
				if d.Memo {
					event = strings.TrimSpace(event + " ⚡memo")
				}
				rows = append(rows, inspectRow{d.State, visitSeen[d.State], d.Usage.InputTokens, d.Usage.OutputTokens, event})
			}
		case journal.HandlerFailed:
			state, _ := ev.Data["state"].(string)
			class, _ := ev.Data["class"].(string)
			if failures[state] == nil {
				failures[state] = map[string]int{}
			}
			failures[state][class]++
		}
	}
	return rows, failures
}

func printInspectRows(rows []inspectRow) {
	fmt.Printf("%s%-16s %-6s %8s %8s  %s%s\n", cBold, "STATE", "VISIT", "TOK IN", "TOK OUT", "EVENT", cReset)
	for _, r := range rows {
		fmt.Printf("%-16s %-6d %8d %8d  %s\n", r.state, r.visit, r.in, r.out, r.event)
	}
}

func printInspectFailures(failures map[string]map[string]int) {
	if len(failures) == 0 {
		return
	}
	fmt.Printf("\n%sfailures%s\n", cBold, cReset)
	states := make([]string, 0, len(failures))
	for s := range failures {
		states = append(states, s)
	}
	sort.Strings(states)
	for _, s := range states {
		for class, n := range failures[s] {
			fmt.Printf("  %-16s %s ×%d\n", s, class, n)
		}
	}
}

func printInspectRouting(events []*journal.Event) {
	fmt.Printf("\n%srouting%s\n", cBold, cReset)
	for _, ev := range events {
		if ev.Type != journal.TransitionFired {
			continue
		}
		from, _ := ev.Data["from"].(string)
		to, _ := ev.Data["to"].(string)
		on, _ := ev.Data["on"].(string)
		guard, _ := ev.Data["guard"].(string)
		cond := ""
		if on != "" {
			cond += " on:" + on
		}
		if guard != "" {
			cond += " when:" + guard
		}
		fmt.Printf("  %s → %s%s%s%s\n", from, to, cDim, cond, cReset)
	}
}

func printInspectParked(p *journal.ParkInfo) {
	if p == nil {
		return
	}
	fmt.Printf("\n%sparked%s at %s since %s", cYellow, cReset, p.State, p.At.Format(time.RFC3339))
	if p.Timeout > 0 {
		fmt.Printf(" (expires %s → %s)", p.At.Add(p.Timeout).Format(time.RFC3339), p.OnTimeout)
	}
	fmt.Println()
}

func printInspectConversations(rs *journal.RunState) {
	states := make([]string, 0, len(rs.Convos))
	for s := range rs.Convos {
		states = append(states, s)
	}
	sort.Strings(states)
	for _, s := range states {
		fmt.Printf("\n%sconversation: %s%s (last execution)\n", cBold, s, cReset)
		for _, m := range rs.Convos[s] {
			text := m.Text
			if flagVerbose < 1 {
				text = strings.Join(strings.Fields(text), " ")
				if len(text) > 200 {
					text = text[:200] + "…"
				}
			}
			fmt.Printf("  [%s] %s\n", m.Role, text)
			for _, tc := range m.ToolCalls {
				fmt.Printf("  [tool_call] %s %v\n", tc.Name, tc.Args)
			}
		}
	}
}
