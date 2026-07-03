// Command steps runs state-machine workflows for micro-agents.
//
//	steps validate workflow.ts [--print]
//	steps run workflow.ts --input article=@fixtures/article.txt [--mock mock.yaml]
//	steps resume <run-id> --event approved [--data '{"note":"ship it"}']
//	steps runs
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

var (
	flagDB           string
	flagVerbose      bool
	flagMock         string
	flagDefaultModel string
)

func main() {
	root := &cobra.Command{
		Use:           "steps",
		Short:         "A state-machine runtime for micro-agents",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagDB, "db", ".steps.db", "journal database path")
	root.PersistentFlags().BoolVarP(&flagVerbose, "verbose", "v", false, "log full prompts, replies, and tool payloads")
	root.PersistentFlags().StringVar(&flagMock, "mock", "", "mock responses YAML — replaces every model with scripted replies")
	root.PersistentFlags().StringVar(&flagDefaultModel, "default-model", os.Getenv("STEPS_DEFAULT_MODEL"), "engine-level default model (last rung of the cascade)")

	root.AddCommand(cmdValidate(), cmdRun(), cmdResume(), cmdRuns(), cmdInspect(), cmdContext())

	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "%serror:%s %v\n", cRed, cReset, err)
		os.Exit(1)
	}
}

func parseOpts() []machine.ParseOption {
	var opts []machine.ParseOption
	if flagDefaultModel != "" {
		opts = append(opts, machine.WithEngineDefaultModel(flagDefaultModel))
	}
	if flagMock != "" {
		// Mock runs do not need resolvable providers; the model field still
		// must exist, so give the cascade a fallback.
		opts = append(opts, machine.WithEngineDefaultModel("mock/scripted"))
	}
	return opts
}

func buildEngine(l engine.Listener) (*engine.Engine, *journal.SQLiteStore, error) {
	store, err := journal.OpenSQLite(flagDB)
	if err != nil {
		return nil, nil, fmt.Errorf("opening journal %s: %w", flagDB, err)
	}
	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), l)
	if flagMock != "" {
		script, err := provider.LoadScript(flagMock)
		if err != nil {
			store.Close()
			return nil, nil, err
		}
		eng.Mock = script
	}
	return eng, store, nil
}

func cmdValidate() *cobra.Command {
	var printExpanded bool
	c := &cobra.Command{
		Use:   "validate <machine.ts>",
		Short: "Validate a machine: structure, schemas, and a stub dry-run of every function",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := machine.Load(args[0], parseOpts()...)
			if err != nil {
				return err
			}
			_, warnings := machine.DryRun(m)
			for _, w := range warnings {
				fmt.Printf("%s⚠%s %s\n", cYellow, cReset, w)
			}
			fmt.Printf("%s✔%s %s is valid (%d states, hash %s)\n", cGreen, cReset, args[0], len(m.States), m.Hash[:12])
			if printExpanded {
				printMachine(m)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&printExpanded, "print", false, "print the fully-expanded machine")
	return c
}

func cmdContext() *cobra.Command {
	var stateName string
	c := &cobra.Command{
		Use:   "context <machine.ts>",
		Short: "Show what each state's functions may reference (derived from declared schemas)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := machine.Load(args[0], parseOpts()...)
			if err != nil {
				return err
			}
			for _, s := range m.States {
				if s.Terminal || (stateName != "" && s.Name != stateName) {
					continue
				}
				fmt.Println(machine.ScopeDoc(m, s))
			}
			return nil
		},
	}
	c.Flags().StringVar(&stateName, "state", "", "limit to one state")
	return c
}

func cmdRun() *cobra.Command {
	var inputs []string
	c := &cobra.Command{
		Use:   "run <workflow.ts>",
		Short: "Start a run and drive it until it finishes or parks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := machine.Load(args[0], parseOpts()...)
			if err != nil {
				return err
			}
			input, err := parseInputs(inputs)
			if err != nil {
				return err
			}
			eng, store, err := buildEngine(&prettyListener{verbose: flagVerbose})
			if err != nil {
				return err
			}
			defer store.Close()

			res, err := eng.Start(context.Background(), m, input)
			if err != nil {
				return err
			}
			return emitResult(res)
		},
	}
	c.Flags().StringArrayVar(&inputs, "input", nil, "run input: key=value or key=@file (repeatable)")
	return c
}

func cmdResume() *cobra.Command {
	var event, dataJSON string
	c := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Resume a parked or crashed run from its journal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, store, err := buildEngine(&prettyListener{verbose: flagVerbose})
			if err != nil {
				return err
			}
			defer store.Close()

			run, err := store.GetRun(context.Background(), args[0])
			if err != nil {
				return err
			}
			// The run is pinned to the machine it started with; include()
			// resolves from pinned assets, never the filesystem.
			m, err := machine.ParseWithAssets(run.Source, run.Assets, parseOpts()...)
			if err != nil {
				return fmt.Errorf("re-evaluating pinned machine: %w", err)
			}

			var data map[string]any
			if dataJSON != "" {
				if err := json.Unmarshal([]byte(dataJSON), &data); err != nil {
					return fmt.Errorf("--data must be a JSON object: %w", err)
				}
			}
			res, err := eng.Resume(context.Background(), m, args[0], event, data)
			if err != nil {
				return err
			}
			return emitResult(res)
		},
	}
	c.Flags().StringVar(&event, "event", "", "event answering a parked human gate")
	c.Flags().StringVar(&dataJSON, "data", "", "JSON object merged as the gate's output")
	return c
}

func cmdRuns() *cobra.Command {
	return &cobra.Command{
		Use:   "runs",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := journal.OpenSQLite(flagDB)
			if err != nil {
				return err
			}
			defer store.Close()
			runs, err := store.ListRuns(context.Background())
			if err != nil {
				return err
			}
			if len(runs) == 0 {
				fmt.Println("no runs")
				return nil
			}
			fmt.Printf("%-18s %-22s %-8s %-14s %s\n", "RUN", "MACHINE", "STATUS", "STATE", "UPDATED")
			for _, r := range runs {
				fmt.Printf("%-18s %-22s %-8s %-14s %s\n",
					r.ID, r.Machine, r.Status, r.CurrentState, r.Updated.Format(time.RFC3339))
			}
			return nil
		},
	}
}

func parseInputs(pairs []string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok {
			return nil, fmt.Errorf("input %q must be key=value or key=@file", p)
		}
		if strings.HasPrefix(v, "@") {
			raw, err := os.ReadFile(v[1:])
			if err != nil {
				return nil, fmt.Errorf("input %s: %w", k, err)
			}
			out[k] = string(raw)
			continue
		}
		out[k] = v
	}
	return out, nil
}

// emitResult prints the machine-readable summary to stdout (the narration
// went to stderr). Failed runs exit non-zero.
func emitResult(res *engine.Result) error {
	summary := map[string]any{
		"run_id":      res.RunID,
		"status":      res.Status,
		"terminal":    res.Terminal,
		"transitions": res.State.Transitions,
		"tokens":      res.State.Usage.Total(),
	}
	raw, _ := json.MarshalIndent(summary, "", "  ")
	fmt.Println(string(raw))
	if res.Status == journal.StatusFailed {
		os.Exit(2)
	}
	return nil
}

func printMachine(m *machine.Machine) {
	fmt.Printf("\nmachine %s%s%s\n", cBold, m.Name, cReset)
	fmt.Printf("initial: %s\n", m.Initial)
	fmt.Printf("limits: max_transitions=%d timeout=%s", m.Limits.MaxTransitions, m.Limits.Timeout)
	if m.Limits.MaxTokens > 0 {
		fmt.Printf(" max_tokens=%d", m.Limits.MaxTokens)
	}
	if m.Limits.MaxCost > 0 {
		fmt.Printf(" max_cost=%.2f", m.Limits.MaxCost)
	}
	fmt.Println()
	fmt.Println("states:")
	for _, s := range m.States {
		if s.Terminal {
			status := "success"
			if s.Status == "failed" {
				status = "failed"
			}
			fmt.Printf("  %s%s%s (terminal, %s)\n", cBold, s.Name, cReset, status)
			continue
		}
		detail := s.HandlerKind()
		switch {
		case s.Agent != nil:
			detail = fmt.Sprintf("agent %s, maxTurns=%d", s.Agent.Model.Display(), s.Agent.MaxTurns)
			if s.Agent.Adopt != "" {
				detail += ", adopt=" + s.Agent.Adopt
			}
		case s.Action != nil:
			detail = "action " + s.Action.Name
		case s.Human != nil:
			detail = "human gate"
			if s.Human.Timeout > 0 {
				detail += fmt.Sprintf(", timeout=%s→%s", s.Human.Timeout, s.Human.OnTimeout)
			}
		}
		fmt.Printf("  %s%s%s (%s)\n", cBold, s.Name, cReset, detail)
		if len(s.Output.Schema) > 0 && !s.Output.DefaultOutput() {
			keys := make([]string, 0, len(s.Output.Schema))
			for k := range s.Output.Schema {
				keys = append(keys, k)
			}
			fmt.Printf("    output: %s", strings.Join(keys, ", "))
			if len(s.Output.Events) > 0 {
				fmt.Printf(" (events: %s)", strings.Join(s.Output.Events, ", "))
			}
			fmt.Println()
		}
		for _, rp := range s.Retry {
			fmt.Printf("    retry: %s ×%d\n", strings.Join(rp.Match, "|"), rp.MaxAttempts)
		}
		for _, t := range s.Transitions {
			cond := ""
			if t.On != "" {
				cond += " on:" + t.On
			}
			if !t.When.IsZero() {
				cond += " when: " + t.When.Display()
			}
			if cond == "" {
				cond = " (fallback)"
			}
			fmt.Printf("    → %s%s\n", t.To, cond)
		}
		for _, ca := range s.Catch {
			fmt.Printf("    catch %s → %s\n", strings.Join(ca.Match, "|"), ca.To)
		}
	}
}
