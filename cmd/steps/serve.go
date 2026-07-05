package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/jtarchie/steps/engine"
)

func cmdServe() *cobra.Command {
	var addr string
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

			e := newServer(store, eng)
			fmt.Fprintf(os.Stderr, "%s▶ steps serve%s  http://%s  %s(journal: %s)%s\n",
				cBold, cReset, addr, cDim, flagDB, cReset)
			return e.Start(addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8484", "address to listen on")
	return c
}
