package cli

// The half of the web UI's mcp tab that internal/web cannot hold: what a saved oauth token is worth, and what a server says when somebody actually connects to it. Both need internal/mcp, which depguard keeps out of that package — the same split web.Manager and web.Authorizer already draw.

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// probes is every live probe this daemon has been asked for, and the last answer each one gave.
type probes struct {
	base    context.Context //nolint:containedctx // a probe outlives the click that started it, and dies with the daemon
	mu      sync.Mutex
	results map[string]*probeResult
	// cancels lets shutdown take an in-flight probe with it, keyed the same way, since a probe's whole cost is a connection nobody is waiting on any more.
	cancels  map[string]context.CancelFunc
	inflight sync.WaitGroup
}

func newProbes(base context.Context) *probes {
	return &probes{base: base, results: map[string]*probeResult{}, cancels: map[string]context.CancelFunc{}}
}

// probeResult is one answer and the configuration it describes. The fingerprint is a FIELD rather than part of the key so this map holds one entry per declared server however often a `steps pipeline set` moves an endpoint under it — keyed by the configuration, it would gain an entry per version ever tested and drop none, which is the leak staleLogins is capped against.
type probeResult struct {
	fingerprint string
	probe       web.MCPProbe
}

// probeKey scopes a result to the pipeline that asked and the server it asked about.
func probeKey(pipeline *web.Pipeline, server string) string {
	return pipeline.Slug + "\x00" + server
}

// probeFingerprint is the configuration an answer describes, and what invalidates it when a `steps pipeline set` moves the endpoint: the old answer describes a server this pipeline no longer has, and a stale "✓ 7 tools" against a moved endpoint is worse than no answer. It needs no hook on set, rename or destroy — a changed configuration simply misses.
func probeFingerprint(srv *config.MCPServer) string {
	return srv.Target() + "\x00" + srv.AuthLabel()
}

// MCPState reports what the token-holder knows about one server. No request is made: the page calls this on every 2.5s poll, for every declared server.
func (p *probes) MCPState(pipeline *web.Pipeline, server string) web.MCPState {
	state := web.MCPState{}

	srv, err := pipeline.Config().FindMCPServer(server)
	if err != nil {
		state.Credential.Detail = err.Error()

		return state
	}

	if srv.Auth.Type == "oauth" {
		token := stepsmcp.InspectToken(*srv)
		state.Credential = web.MCPCredential{Connected: token.Connected, Detail: token.Detail}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if held := p.results[probeKey(pipeline, server)]; held != nil && held.fingerprint == probeFingerprint(srv) {
		probe := held.probe
		state.Probe = &probe
	}

	return state
}

// StartProbe connects to one server in the background. Detached rather than inline because a preflight timeout is thirty seconds by default and a page is never otherwise slow: the click comes straight back, the row says it is testing, and the poll that already drives the page swaps in the answer.
func (p *probes) StartProbe(pipeline *web.Pipeline, server string) error {
	srv, err := pipeline.Config().FindMCPServer(server)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	if skip := srv.NotProbableHere(); skip != "" {
		return fmt.Errorf("mcp server %q cannot be probed from here: %s", server, skip)
	}

	key := probeKey(pipeline, server)
	fingerprint := probeFingerprint(srv)
	timeout := pipeline.Config().PreflightSettings().ProbeTimeout()

	p.mu.Lock()

	// A second click while one is in flight is somebody being impatient, not a second question. Answering it would open a second connection to the same server to learn the same thing.
	if held := p.results[key]; held != nil && held.fingerprint == fingerprint && held.probe.Running {
		p.mu.Unlock()

		return nil
	}

	ctx, cancel := context.WithTimeout(p.base, timeout)

	// A probe of the configuration this one replaces is answering a question nobody is asking any more.
	if previous := p.cancels[key]; previous != nil {
		previous()
	}

	mine := &probeResult{fingerprint: fingerprint, probe: web.MCPProbe{Running: true, At: time.Now()}}
	p.results[key] = mine
	p.cancels[key] = cancel
	p.mu.Unlock()

	target := *srv

	p.inflight.Go(func() {
		defer cancel()

		tools, probeErr := stepsmcp.ListServerTools(ctx, target)
		result := web.MCPProbe{At: time.Now()}

		if probeErr != nil {
			result.Detail = config.MCPStatusReason(server, probeErr)
		} else {
			result.OK = true
			result.Detail = fmt.Sprintf("%d %s", len(tools), pluralize(len(tools), "tool"))
		}

		p.mu.Lock()

		// Only while this is still the probe the row waits on: a cancelled predecessor unwinds AFTER its replacement has recorded itself, and letting it write would report its own cancellation as the newer question's answer.
		if p.results[key] == mine {
			p.results[key] = &probeResult{fingerprint: fingerprint, probe: result}
			delete(p.cancels, key)
		}

		p.mu.Unlock()
	})

	return nil
}

// stopProbes takes every probe still in flight down with the daemon, which is what keeps goleak quiet about a connection nobody is waiting on.
func (p *probes) stopProbes() {
	p.mu.Lock()
	for _, cancel := range p.cancels {
		cancel()
	}
	p.mu.Unlock()

	p.inflight.Wait()
}
