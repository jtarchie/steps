package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// probeAgentServers checks the MCP servers one agent's tool grant names.
func probeAgentServers(ctx context.Context, cfg *config.Config, ri config.ResolvedInvocation, settings *config.Preflight, seen map[string]bool) []config.Problem {
	var problems []config.Problem

	for _, spec := range ri.ToolSpecs {
		// Deduped by GRANT, not by server. Deduping by name meant the
		// second grant on a server never reached the cache at all, so
		// whether a bad tool name was caught came down to which agent was
		// listed first. cfg is fixed for one Preflight, so the grant is the
		// whole identity here — the definition belongs in the cache key,
		// which outlives this call.
		if spec.MCP == "" {
			continue
		}

		key := spec.MCP + "|" + spec.MCPTool + "|" + strings.Join(spec.MCPTools, ",") + "|" + pinsKey(spec)
		if seen[key] {
			continue
		}

		seen[key] = true

		err := probeServerCached(ctx, cfg, spec, settings)
		if err != nil {
			problems = append(problems, config.Problem{
				Target:    fmt.Sprintf("mcp %q", spec.MCP),
				Detail:    err.Error(),
				Transient: isTransient(err),
			})
		}
	}

	return problems
}

func probeServerCached(ctx context.Context, cfg *config.Config, spec config.ToolSpec, settings *config.Preflight) error {
	// MCPTool belongs in the key as much as MCPTools does, and leaving it out
	// was a hole in exactly the check this function performs. probeServer
	// asks two questions — does the server answer, and does it expose the
	// tools THIS grant names — and the second one has a different answer per
	// grant. Keyed without MCPTool, `{mcp: x, tool: a}` and `{mcp: x, tool:
	// b}` shared one entry, so whichever ran first answered for both: a grant
	// naming a tool the server does not expose passed preflight on the
	// strength of a different grant's tool existing. Silent, and only in a
	// process that had already probed the same server — which is every
	// `steps web`.
	// The DEFINITION, not just the name. The cache is process-wide and
	// pipeline-blind, and a name is not an identity: under
	// one daemon holding several pipelines two pipelines may each declare a server
	// called `test` pointing at different things, and one verdict served
	// both — a healthy server vouching for a broken neighbour, and a broken
	// one condemning a healthy neighbour. Naming the definition is stronger
	// than naming the pipeline: it also separates two revisions of one
	// pipeline. internal/resource/preflight.go keys its sibling cache the
	// same way and was immune to both.
	// Except for oauth: the definition no longer identifies that connection,
	// because the credential it sends is the PIPELINE's saved login, so
	// one pipeline's login must not vouch for another's missing one.
	srv, err := cfg.FindMCPServer(spec.MCP)
	if err != nil {
		// A name no server answers to is a config error, identical on every
		// call, and there is no definition to key it under.
		return err //nolint:wrapcheck // FindMCPServer already names the server and lists the alternatives
	}

	identity := []string{"mcp", srv.Name, srv.Endpoint, srv.Command, srv.Cwd}
	if srv.Auth.Type == "oauth" {
		identity = append(identity, cfg.Name)
	}

	key := strings.Join(append(
		identity,
		append(append([]string{}, srv.Args...), spec.MCPTool, strings.Join(spec.MCPTools, ","), pinsKey(spec))...,
	), "|")
	now := time.Now()

	entry, found := probeCache.lookup(key, settings.CacheWindow(), now)
	if found {
		slog.DebugContext(ctx, "preflight.cached", "target", key)

		return entry.err
	}

	err = probeServer(ctx, cfg.Name, *srv, spec, settings.ProbeTimeout())

	// A probe the CALLER abandoned learned nothing about the target. The
	// probe's own deadline firing IS a fact about the target and is cached;
	// ctx being done means Ctrl-C, a job deadline, or a shutdown, and
	// describeProbeError renders both identically, so without this an
	// aborted build left a healthy server looking dead for the whole cache
	// window.
	if ctx.Err() != nil {
		return err
	}

	// A missing, mis-endpointed or unrenewable oauth token is fixed OUT OF
	// BAND, seconds later, by the one actor this process cannot observe: a
	// human running `steps mcp login`, or clicking Connect on the daemon's
	// own mcp tab. Remembering that verdict makes the daemon disbelieve work
	// that has already been done — the login succeeds, the tab says
	// authorized, the login is saved, and every poll for the rest of
	// the window still fails quoting a stat that stopped being true. Nothing
	// else clears it either: ResetProbeCache has no caller outside tests, so
	// the only cure was restarting the daemon.
	//
	// It is also the cheapest failure here to re-establish. checkCredential
	// answers it by reading one file, before a connection is opened, so
	// declining to cache it costs a stat per poll and nothing else — unlike
	// the server-did-not-answer failures below it, which cost a round trip.
	//
	// Deliberately NOT the same question as transient(), which leaves this
	// class terminal so a watcher refuses the run rather than polling a dead
	// credential forever. Whether to RUN and whether to REMEMBER are
	// different: the run should still be refused now, and the refusal should
	// still be re-derived next time.
	if errors.Is(err, stepsmcp.ErrNeedsLogin) {
		return err
	}

	probeCache.store(key, err, now)

	return err
}

// probeServer starts an MCP server, confirms it comes up, and confirms the
// tools the pipeline grants actually exist on it. The second half matters as
// much as the first: a server that starts but no longer exposes
// `go_symbol_references` fails the step just as surely as one that never
// started, and just as late.
func probeServer(ctx context.Context, pipeline string, srv config.MCPServer, spec config.ToolSpec, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tools, err := stepsmcp.ListServerTools(probeCtx, pipeline, srv)
	if err != nil {
		if probeCtx.Err() != nil {
			return transient(fmt.Errorf("did not start within %s", timeout))
		}

		// Transient by default: a server that did not come up is usually a
		// fact about this minute. transient() itself makes the exception for
		// an oauth token no poll can renew, which is the case that has to
		// stop a watcher rather than be retried by it.
		return transient(fmt.Errorf("could not start: %w", err))
	}

	selected, err := selectMCPTools(spec, tools)
	if err != nil {
		// selectMCPTools already names the missing tool and lists what the
		// server does offer.
		return err
	}

	// A pin that cannot bind is a config error, not a fact about this
	// minute, so it stays unmarked: a watcher refuses rather than retries.
	if spec.MCPTool != "" {
		_, _, err = pinMCPArgs(spec, selected[0])
	}

	return err
}

// pinsKey folds a grant's args: pins into a probe key. The verdict on a pin
// is per grant, so without it one agent's good pin vouched for another's bad
// one on the same tool — the same hole the MCPTool comment in
// probeServerCached describes.
func pinsKey(spec config.ToolSpec) string {
	pairs := make([]string, 0, len(spec.Args))
	for _, key := range sortedKeys(spec.Args) {
		pairs = append(pairs, key+"="+spec.Args[key])
	}

	return strings.Join(pairs, ",")
}
