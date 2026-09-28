package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/workspace"
)

// ValidateCmd checks a pipeline and prints what's wrong with it, without
// running any of it.
//
// Every check it performs already existed; the only way to reach them was to
// start a run, which opens a state store, builds a workspace, preflights
// docker, and then begins executing steps. That made "is my YAML right?" an
// expensive, side-effecting question — worst while writing the pipeline, which
// is exactly when it gets asked. This command answers it with no store, no
// workspace, no containers, and nothing written to disk.
type ValidateCmd struct {
	VarFlags `embed:""`
	Pipeline string `arg:""   help:"path to the pipeline YAML file"`
	// SyntaxOnly skips the checks about THIS MACHINE — credentials and MCP
	// binaries — leaving only the checks about the file. It exists for the
	// lint-in-CI case: a pre-commit hook or a build that checks a pipeline it
	// has no intention of running should not need that pipeline's production
	// credentials on hand.
	SyntaxOnly bool `help:"check the file only; skip credential and MCP-binary checks about this machine" name:"syntax-only"`
	// Live goes the other way: past what is knowable locally, out to the
	// models and MCP servers themselves. It was `steps preflight`, which is
	// the same read at a different depth — and a verb whose only difference
	// from this one was how far it looked.
	Live bool   `help:"also probe the models and MCP servers, live"                                    name:"live"`
	Job  string `help:"with --live, probe only this job's models and MCP servers (default: every job)"`
	// Name is here because --live dials oauth servers with the pipeline's own login, which is filed under this name.
	Name map[string]string `help:"name the pipeline a file's mcp logins are filed under, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
}

// Run loads the pipeline (which runs every config-level validator) and then
// checks artifact flow for each job, joining the failures so one invocation
// reports everything wrong with the file.
func (v *ValidateCmd) Run() error {
	err := v.checkDepth()
	if err != nil {
		return err
	}

	cfg, err := v.Load(v.Pipeline, resolvePipelineName(v.Pipeline, v.Name))
	if err != nil {
		return err
	}

	err = fileProblems(cfg)
	if err != nil {
		return err
	}

	problems, err := v.machineProblems(cfg)
	if err != nil {
		return err
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s cannot run here:\n%s", v.Pipeline, renderProblems(problems))
	}

	fmt.Printf("ok: %s (%d job(s), %d resource(s), %d agent(s))%s\n",
		v.Pipeline, len(cfg.Jobs), len(cfg.Resources), len(cfg.Agents), liveNote(v.Live))

	return nil
}

// fileProblems is the shallowest depth: what is wrong with the file itself,
// knowable without this machine and without any service.
//
// An unparsable expr: expression is one of these, so it is checked before
// --syntax-only can skip anything — nothing about it depends on where it runs.
func fileProblems(cfg *config.Config) error {
	var errs []error

	exprErr := pipeline.ValidateExpressions(cfg)
	if exprErr != nil {
		errs = append(errs, exprErr)
	}

	for i := range cfg.Jobs {
		flowErr := workspace.ValidateArtifactFlow(cfg, &cfg.Jobs[i])
		if flowErr != nil {
			errs = append(errs, flowErr)
		}
	}

	return errors.Join(errs...)
}

// machineProblems is the other two depths: what this machine cannot supply,
// and — under --live — what the services themselves say when asked.
//
// "ok" has to mean "this will run", not "the YAML parses". The credentials
// agents need and the binaries MCP servers need are knowable in microseconds
// and were, before this command, discovered at run time, after an agent step
// had already started billing. They live here rather than in LoadConfig
// deliberately: an absent key is a fact about this machine right now, so
// making it a load error would break `steps plan` on a laptop and any CI job
// that lints a pipeline it does not run.
//
// Live only when the local checks passed: a missing API key makes every probe
// that would use it fail too, and reporting both would be reporting one
// problem twice.
func (v *ValidateCmd) machineProblems(cfg *config.Config) ([]config.Problem, error) {
	if v.SyntaxOnly {
		return nil, nil
	}

	problems := cfg.CheckEnvironment()
	if len(problems) > 0 || !v.Live {
		return problems, nil
	}

	return v.liveProblems(cfg)
}

// checkDepth refuses the two flag combinations that contradict each other.
//
// The three depths are ordered — the file, then this machine, then the
// services — so asking for the shallowest and the deepest at once is not a
// preference to resolve but a sentence that means nothing. And --job is only
// a narrowing of the live probe: without --live it would read as configured
// and bind nothing, which is the shape this codebase rejects everywhere.
func (v *ValidateCmd) checkDepth() error {
	if v.Live && v.SyntaxOnly {
		return errors.New("--live and --syntax-only ask for opposite depths: --syntax-only checks the file alone, --live checks the file, this machine, and the services behind it")
	}

	if v.Job != "" && !v.Live {
		return errors.New("--job narrows the live probe: pass --live with it, or drop it to check the whole file")
	}

	return nil
}

// liveProblems probes the models and MCP servers themselves — what `steps
// preflight` was.
//
// Every job unless --job names one, because "is this pipeline runnable right
// now" is a question about the file, and a validate that quietly checked one
// job would answer a narrower question than it was asked. The probes are
// cached for the process, so the jobs that share a model pay for it once.
func (v *ValidateCmd) liveProblems(cfg *config.Config) ([]config.Problem, error) {
	// Refused rather than answered emptily: the probes below return no
	// problems when the pipeline has turned the check off, and the ok line
	// would then vouch that every model and MCP server responded having
	// contacted none of them. --live is the one depth whose whole claim is
	// that something was asked.
	if !cfg.PreflightSettings().Enabled() {
		return nil, errors.New("--live cannot probe: this pipeline sets defaults.preflight.disabled: true, so there is nothing to ask; drop --live, or turn the check back on")
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	if v.Job != "" {
		job, err := cfg.FindJob(v.Job)
		if err != nil {
			return nil, fmt.Errorf("cannot probe: %w", err)
		}

		return pipeline.Preflight(ctx, cfg, job), nil
	}

	names := make([]string, 0, len(cfg.Resources))
	for _, resource := range cfg.Resources {
		names = append(names, resource.Name)
	}

	return pipeline.PreflightPipeline(ctx, cfg, names), nil
}

// liveNote says which depth the ok line is vouching for, since the two read
// identically otherwise and only one of them talked to anything.
func liveNote(live bool) string {
	if live {
		return " — every model and MCP server responded"
	}

	return ""
}

// renderProblems lays out preflight problems one per line, target-first, so a
// pipeline with several is read as a checklist rather than as prose. Reporting
// all of them is the point: finding them one run at a time is the failure mode
// this exists to end.
func renderProblems(problems []config.Problem) string {
	var out strings.Builder

	width := 0
	for _, problem := range problems {
		width = max(width, len(problem.Target))
	}

	for _, problem := range problems {
		fmt.Fprintf(&out, "  %-*s  %s\n", width, problem.Target, problem.Detail)
	}

	return strings.TrimRight(out.String(), "\n")
}
