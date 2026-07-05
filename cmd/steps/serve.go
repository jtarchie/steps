package main

import (
	"context"
	"fmt"
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
	var hookInputs []string
	var hookTokens []string
	var maxInFlight int
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

			hooks, err := loadHooks(hookPaths, hookInputs, hookTokens)
			if err != nil {
				return err
			}

			// A cancellable context bounds the dispatcher goroutine to the
			// server's lifetime.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			e := newServer(ctx, store, eng, hooks, maxInFlight)
			fmt.Fprintf(os.Stderr, "%s▶ steps serve%s  http://%s  %s(journal: %s)%s\n",
				cBold, cReset, addr, cDim, flagDB, cReset)
			return e.Start(addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", "127.0.0.1:8484", "address to listen on")
	c.Flags().StringArrayVar(&hookPaths, "hook", nil, "workflow.ts with a webhook: block — POST /hooks/<path> queues a run (repeatable)")
	c.Flags().StringArrayVar(&hookInputs, "hook-input", nil, "fixed run input for webhook-started runs: key=value or key=@file (repeatable)")
	c.Flags().StringArrayVar(&hookTokens, "hook-token", nil, "shared secret: path=secret scopes it to one hook, a bare value is the fallback for all (repeatable)")
	c.Flags().IntVar(&maxInFlight, "max-in-flight", runtime.NumCPU(), "global cap on concurrently executing webhook runs across all hooks")
	return c
}

// loadHooks parses each --hook workflow into a hookSpec keyed by its webhook
// path, resolving per-hook tokens and rejecting duplicate paths or machine
// names (the dispatcher keys per-hook limits by machine name).
func loadHooks(paths, inputs, tokenPairs []string) (map[string]*hookSpec, error) {
	hooks := map[string]*hookSpec{}
	if len(paths) == 0 {
		return hooks, nil
	}
	base, err := parseInputs(inputs)
	if err != nil {
		return nil, err
	}
	tokens, fallback := parseHookTokens(tokenPairs)

	byName := map[string]string{}
	for _, path := range paths {
		m, err := machine.Load(path, parseOpts()...)
		if err != nil {
			return nil, fmt.Errorf("loading hook machine %s: %w", path, err)
		}
		if m.Webhook == nil {
			return nil, fmt.Errorf("machine %s declares no webhook: block — add `webhook: {path, map}` to accept triggers", path)
		}
		slug := m.Webhook.Path
		if _, dup := hooks[slug]; dup {
			return nil, fmt.Errorf("duplicate webhook path %q (from %s) — each hook needs a unique path", slug, path)
		}
		if prev, dup := byName[m.Name]; dup {
			return nil, fmt.Errorf("duplicate machine name %q from %s and %s — each hook needs a unique name", m.Name, prev, path)
		}
		byName[m.Name] = path

		token := fallback
		if t, ok := tokens[slug]; ok {
			token = t
		}
		hooks[slug] = &hookSpec{m: m, inputs: base, token: token}
		fmt.Fprintf(os.Stderr, "%s▶ hook%s  POST /hooks/%s  →  %s  %s(maxInFlight=%d, maxQueued=%d)%s\n",
			cBold, cReset, slug, m.Name, cDim,
			resolveDefault(m.Webhook.MaxInFlight, machine.DefaultHookMaxInFlight),
			resolveDefault(m.Webhook.MaxQueued, machine.DefaultHookMaxQueued), cReset)
	}
	return hooks, nil
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

func resolveDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
