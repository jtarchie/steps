package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/machine"
)

func cmdServe() *cobra.Command {
	var addr string
	var hookPaths []string
	var machinePaths []string
	var hookInputs []string
	var hookTokens []string
	var maxInFlight int
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve a web view of runs, trigger machines, and answer parked gates from the browser",
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

			reg, err := loadServed(hookPaths, machinePaths, hookInputs, hookTokens)
			if err != nil {
				return err
			}

			// A cancellable context bounds the dispatcher goroutine to the
			// server's lifetime.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			e := newServer(ctx, store, eng, reg, maxInFlight)
			fmt.Fprintf(os.Stderr, "%s▶ steps serve%s  http://%s  %s(journal: %s)%s\n",
				cBold, cReset, addr, cDim, flagDB, cReset)
			return e.Start(addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8484", "address to listen on")
	c.Flags().StringArrayVar(&hookPaths, "hook", nil, "workflow.ts with a webhook: block — POST /hooks/<path> queues a run, and it's manually triggerable too (repeatable)")
	c.Flags().StringArrayVar(&machinePaths, "machine", nil, "workflow.ts to expose for manual triggering at /machines/<name> — no webhook: block required (repeatable)")
	c.Flags().StringArrayVar(&hookInputs, "hook-input", nil, "fixed run input for served machines: key=value or key=@file (repeatable)")
	c.Flags().StringArrayVar(&hookTokens, "hook-token", nil, "shared secret: path=secret scopes it to one hook, a bare value is the fallback for all (repeatable)")
	c.Flags().IntVar(&maxInFlight, "max-in-flight", runtime.NumCPU(), "global cap on concurrently executing runs across all served machines")
	return c
}

// loadServed builds the registry of machines exposed by this serve: the --hook
// machines (webhook-triggerable and manually triggerable) plus the --machine
// machines (manual only). --hook-input base values attach to every entry.
// Machine names must be unique across both flag sets, since the dispatcher and
// trigger routes key by name.
func loadServed(hookPaths, machinePaths, inputs, tokenPairs []string) (*served, error) {
	reg := &served{
		byName: map[string]*servedMachine{},
		byPath: map[string]*servedMachine{},
	}
	if len(hookPaths) == 0 && len(machinePaths) == 0 {
		return reg, nil
	}
	base, err := parseInputs(inputs)
	if err != nil {
		return nil, err
	}
	tokens, fallback := parseHookTokens(tokenPairs)
	fromFile := map[string]string{} // machine name -> source path, for duplicate errors

	for _, path := range hookPaths {
		m, err := machine.Load(path, parseOpts()...)
		if err != nil {
			return nil, fmt.Errorf("loading hook machine %s: %w", path, err)
		}
		if m.Webhook == nil {
			return nil, fmt.Errorf("machine %s declares no webhook: block — add `webhook: {path, map}`, or expose it with --machine for manual triggering only", path)
		}
		slug := m.Webhook.Path
		if _, dup := reg.byPath[slug]; dup {
			return nil, fmt.Errorf("duplicate webhook path %q (from %s) — each hook needs a unique path", slug, path)
		}
		if prev, dup := fromFile[m.Name]; dup {
			return nil, fmt.Errorf("duplicate machine name %q from %s and %s — each served machine needs a unique name", m.Name, prev, path)
		}
		fromFile[m.Name] = path

		token := fallback
		if t, ok := tokens[slug]; ok {
			token = t
		}
		sm := &servedMachine{m: m, inputs: base, token: token}
		reg.byPath[slug] = sm
		reg.byName[m.Name] = sm
		fmt.Fprintf(os.Stderr, "%s▶ hook%s  POST /hooks/%s  →  %s  %s(maxInFlight=%d, maxQueued=%d)%s\n",
			cBold, cReset, slug, m.Name, cDim, sm.maxInFlight(), sm.maxQueued(), cReset)
	}

	for _, path := range machinePaths {
		m, err := machine.Load(path, parseOpts()...)
		if err != nil {
			return nil, fmt.Errorf("loading machine %s: %w", path, err)
		}
		if prev, dup := fromFile[m.Name]; dup {
			return nil, fmt.Errorf("duplicate machine name %q from %s and %s — a --hook machine is already manually triggerable, so it needn't be passed to --machine as well", m.Name, prev, path)
		}
		fromFile[m.Name] = path

		sm := &servedMachine{m: m, inputs: base}
		reg.byName[m.Name] = sm
		fmt.Fprintf(os.Stderr, "%s▶ machine%s  GET /machines/%s  →  %s%s\n",
			cBold, cReset, url.PathEscape(m.Name), m.Name, cReset)
	}

	return reg, nil
}

// parseHookTokens splits repeatable --hook-token values into per-path secrets
// (path=secret) and a single bare fallback used by hooks without their own.
func parseHookTokens(pairs []string) (map[string]string, string) {
	tokens := map[string]string{}
	fallback := ""
	for _, p := range pairs {
		if slug, secret, ok := strings.Cut(p, "="); ok {
			tokens[slug] = secret
		} else {
			fallback = p
		}
	}
	return tokens, fallback
}
