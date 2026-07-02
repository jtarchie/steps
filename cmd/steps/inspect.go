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
			store, err := journal.OpenSQLite(flagDB)
			if err != nil {
				return err
			}
			defer store.Close()

			run, err := store.GetRun(context.Background(), args[0])
			if err != nil {
				return err
			}
			events, err := store.Events(context.Background(), args[0])
			if err != nil {
				return err
			}
			rs := journal.Fold(events)

			fmt.Printf("%srun %s%s  %s  %s(%s, hash %s)%s\n",
				cBold, run.ID, cReset, run.Machine, cDim, run.Status, run.Hash[:12], cReset)
			fmt.Printf("state: %s   transitions: %d   tokens: %d in / %d out\n\n",
				rs.Current, rs.Transitions, rs.Usage.InputTokens, rs.Usage.OutputTokens)

			// Per-state execution rows, in journal order.
			type row struct {
				state   string
				visit   int
				in, out int
				event   string
			}
			var rows []row
			visitSeen := map[string]int{}
			failures := map[string]map[string]int{} // state -> class -> count
			for _, ev := range events {
				switch ev.Type {
				case journal.HandlerFinished:
					var d struct {
						State string        `json:"state"`
						Event string        `json:"event"`
						Usage journal.Usage `json:"usage"`
					}
					if journal.DecodeData(ev, &d) == nil {
						visitSeen[d.State]++
						rows = append(rows, row{d.State, visitSeen[d.State], d.Usage.InputTokens, d.Usage.OutputTokens, d.Event})
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

			fmt.Printf("%s%-16s %-6s %8s %8s  %s%s\n", cBold, "STATE", "VISIT", "TOK IN", "TOK OUT", "EVENT", cReset)
			for _, r := range rows {
				fmt.Printf("%-16s %-6d %8d %8d  %s\n", r.state, r.visit, r.in, r.out, r.event)
			}

			if len(failures) > 0 {
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

			if p := rs.Parked; p != nil {
				fmt.Printf("\n%sparked%s at %s since %s", cYellow, cReset, p.State, p.At.Format(time.RFC3339))
				if p.Timeout > 0 {
					fmt.Printf(" (expires %s → %s)", p.At.Add(p.Timeout).Format(time.RFC3339), p.OnTimeout)
				}
				fmt.Println()
			}

			if showMessages {
				states := make([]string, 0, len(rs.Convos))
				for s := range rs.Convos {
					states = append(states, s)
				}
				sort.Strings(states)
				for _, s := range states {
					fmt.Printf("\n%sconversation: %s%s (last execution)\n", cBold, s, cReset)
					for _, m := range rs.Convos[s] {
						text := m.Text
						if !flagVerbose {
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
			return nil
		},
	}
	c.Flags().BoolVar(&showMessages, "messages", false, "dump each state's recorded conversation")
	return c
}
