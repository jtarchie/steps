package pipeline

// Preflight: proving the models and MCP servers a run needs are reachable
// before anything is spent.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/agent"
	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
)

// preflightKey types the context value that switches preflight off.
type preflightKey struct{}

// WithoutPreflight disables the pre-run health check for this invocation,
// backing the --no-preflight flag. On the context rather than a RunJob
// parameter because every caller in the chain would otherwise have to thread a
// flag it has no opinion about.
func WithoutPreflight(ctx context.Context) context.Context {
	return context.WithValue(ctx, preflightKey{}, true)
}

func preflightDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(preflightKey{}).(bool)

	return disabled
}

func preflightSettings(cfg *config.Config) *config.Preflight {
	if cfg.Defaults == nil {
		return nil
	}

	return cfg.Defaults.Preflight
}

// Preflight probes every model and MCP server the job's plan reaches and
// reports the ones that are not working. It runs nothing and changes nothing.
//
// Exported so `steps validate --live` can ask the question without committing to a
// run. The CLI layer reaches internal/agent through here rather than directly,
// keeping the dependency direction the depguard rules describe.
func Preflight(ctx context.Context, cfg *config.Config, job *config.Job) []config.Problem {
	settings := preflightSettings(cfg)
	if !settings.Enabled() {
		return nil
	}

	var problems []config.Problem

	if names := job.AgentNames(); len(names) > 0 {
		problems = agent.Preflight(ctx, cfg, names, settings)
	}

	// The resources a job touches are the other half of "can this job work at
	// all": an mcp-backed resource type calls a remote tool exactly as an
	// agent does, and gets it wrong in exactly the same two ways — a tool that
	// is not there, and arguments that do not satisfy it.
	return append(problems, rsrc.Preflight(ctx, cfg, job, job.ResourceNames(), settings)...)
}

// PreflightPipeline probes everything remote a watcher will eventually depend
// on — the models and MCP servers behind EVERY job's agents, and the
// mcp-backed resource types behind the named trigger resources — the question
// `steps web` must answer before its first poll, where there is no single
// job to preflight.
//
// Both halves, not just the resources, because a watcher's whole shape is
// "wait, then run something nobody is watching". A pipeline whose agent needs
// an unreachable MCP server starts clean, polls clean, and fails at the first
// real trigger — after the get has run, which is both the least useful moment
// to learn it and the one where the failure reads as being about the trigger
// rather than the agent.
func PreflightPipeline(ctx context.Context, cfg *config.Config, names []string) []config.Problem {
	if preflightDisabled(ctx) {
		return nil
	}

	settings := preflightSettings(cfg)
	if !settings.Enabled() {
		return nil
	}

	var problems []config.Problem

	if agents := watchedAgents(cfg); len(agents) > 0 {
		problems = agent.Preflight(ctx, cfg, agents, settings)
	}

	// No job: watch is going to run every one of them, so every put in the
	// pipeline is fair game to judge (see rsrc.Preflight).
	return append(problems, rsrc.Preflight(ctx, cfg, nil, names, settings)...)
}

// watchedAgents is every agent any job's plan invokes, deduped in first-seen
// order. A watcher may run any job, so scoping to one job's agents — what the
// per-job Preflight above does — would be the wrong question here.
func watchedAgents(cfg *config.Config) []string {
	var (
		names []string
		seen  = map[string]bool{}
	)

	for i := range cfg.Jobs {
		for _, name := range cfg.Jobs[i].AgentNames() {
			if seen[name] {
				continue
			}

			seen[name] = true

			names = append(names, name)
		}
	}

	return names
}

// preflight proves the models and MCP servers this job's plan needs are
// actually working, before a single step runs.
//
// The failure it exists for is a plan like plan -> code -> check -> review ->
// publish discovering, half an hour and real money in, that a model was never
// going to answer. Under `steps web` it is worse: nobody is watching, and a
// job re-triggers against a dead model indefinitely.
//
// A job with no agent steps checks nothing and costs nothing.
func preflight(ctx context.Context, cfg *config.Config, job *config.Job) error {
	if preflightDisabled(ctx) {
		return nil
	}

	problems := Preflight(ctx, cfg, job)
	if len(problems) == 0 {
		return nil
	}

	// Explicitly "no steps were run": the whole value of failing here rather
	// than mid-plan is that nothing was spent, and the message has to say so
	// or a reader cannot tell this from an ordinary step failure.
	var out strings.Builder

	fmt.Fprintf(&out, "job %q: preflight failed, no steps were run:", job.Name)

	for _, problem := range problems {
		fmt.Fprintf(&out, "\n  %s: %s", problem.Target, problem.Detail)
	}

	return errors.New(out.String())
}

// ResetPreflightCache forgets everything preflight has verified in this
// process. Tests use it to stay independent of each other; nothing in a real
// run needs it, since the cache is bounded by its own TTL.
func ResetPreflightCache() {
	agent.ResetProbeCache()
	rsrc.ResetPreflightCache()
}
