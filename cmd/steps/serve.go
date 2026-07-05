package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/machine"
)

func cmdServe() *cobra.Command {
	var addr string
	var hookPath string
	var hookInputs []string
	var hookToken string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve a web view of runs and answer parked gates from the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A capturing listener isn't useful here — the durable record is
			// the journal, which the pages read directly. Resume narration
			// (and any failure) is journaled; Warn goes to the server log.
			eng, store, err := buildEngine(engine.NopListener{})
			if err != nil {
				return err
			}
			defer store.Close()

			var hook *hookSpec
			if hookPath != "" {
				m, err := machine.Load(hookPath, parseOpts()...)
				if err != nil {
					return fmt.Errorf("loading hook machine %s: %w", hookPath, err)
				}
				if m.Webhook == nil {
					return fmt.Errorf("machine %s declares no webhook: block — add `webhook: {path, map}` to accept triggers", hookPath)
				}
				base, err := parseInputs(hookInputs)
				if err != nil {
					return err
				}
				hook = &hookSpec{m: m, inputs: base, token: hookToken}
				fmt.Fprintf(os.Stderr, "%s▶ hook%s  POST /hooks/%s  →  %s\n", cBold, cReset, m.Webhook.Path, m.Name)
			}

			e := newServer(store, eng, hook)
			fmt.Fprintf(os.Stderr, "%s▶ steps serve%s  http://%s  %s(journal: %s)%s\n",
				cBold, cReset, addr, cDim, flagDB, cReset)
			return e.Start(addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8484", "address to listen on")
	c.Flags().StringVar(&hookPath, "hook", "", "workflow.ts with a webhook: block — POST /hooks/<path> starts a run")
	c.Flags().StringArrayVar(&hookInputs, "hook-input", nil, "fixed run input for webhook-started runs: key=value or key=@file (repeatable)")
	c.Flags().StringVar(&hookToken, "hook-token", "", "shared secret; POSTs must send Authorization: Bearer <v> or ?token=<v>")
	return c
}
