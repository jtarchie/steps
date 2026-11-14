package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/secret"
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
	// Config is the daemon's file, never read from STEPS_CONFIG (see WebCmd.BeforeResolve): resolving a reference runs the source commands it names, which only an explicit flag should start.
	Config string `help:"with --live, the daemon's config file (steps web --config): every secret reference is resolved as the daemon would, and the path that answered is shown, never the value" name:"config" placeholder:"FILE" type:"path"`
	// Name is here because --live dials oauth servers with the pipeline's own login, which is filed under this name, and --config renders it as {{ .pipeline }} in every lookup path.
	Name map[string]string `help:"name the pipeline is set as: what its mcp logins are filed under and what {{ .pipeline }} renders as in a secret lookup path, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
	DB   DB                `help:"state database the logins are sealed into: a sqlite file path, sqlite:// or postgres:// url (default: .steps/<pipeline>.db beside the YAML)"                                     name:"db"   placeholder:"URL"`
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

	_, err = useLogins(StatePath(v.Pipeline, v.DB), true)
	if err != nil {
		return err
	}

	for _, warning := range pipeline.UnquotedShellValues(cfg) {
		fmt.Fprintln(os.Stderr, "warning: "+warning)
	}

	err = fileProblems(cfg)
	if err != nil {
		return err
	}

	problems, secrets, err := v.machineProblems(cfg)
	if err != nil {
		return err
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s cannot run here:\n%s", v.Pipeline, renderProblems(problems))
	}

	// The @builtin/ agents every pipeline is given are not the pipeline's to count.
	declared := 0

	for _, agent := range cfg.Agents {
		if !strings.HasPrefix(agent.Name, "@builtin/") {
			declared++
		}
	}

	fmt.Printf("ok: %s (%d job(s), %d resource(s), %d agent(s))%s\n",
		v.Pipeline, len(cfg.Jobs), len(cfg.Resources), declared, liveNote(v.Live, secrets))

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

	exportErr := pipeline.ValidateExports(cfg)
	if exportErr != nil {
		errs = append(errs, exportErr)
	}

	for i := range cfg.Jobs {
		flowErr := config.ValidateArtifactFlow(cfg, &cfg.Jobs[i])
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
func (v *ValidateCmd) machineProblems(cfg *config.Config) ([]config.Problem, int, error) {
	if v.SyntaxOnly {
		return nil, 0, nil
	}

	problems := cfg.CheckEnvironment()
	if len(problems) > 0 || !v.Live {
		return problems, 0, nil
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

	if v.Config != "" && !v.Live {
		return errors.New("--config gives the live probe the daemon's secret sources: pass --live with it, or drop it to check the file alone")
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
func (v *ValidateCmd) liveProblems(cfg *config.Config) ([]config.Problem, int, error) {
	// Refused rather than answered emptily: the probes below return no
	// problems when the pipeline has turned the check off, and the ok line
	// would then vouch that every model and MCP server responded having
	// contacted none of them. --live is the one depth whose whole claim is
	// that something was asked.
	if !cfg.PreflightSettings().Enabled() {
		return nil, 0, errors.New("--live cannot probe: this pipeline sets defaults.preflight.disabled: true, so there is nothing to ask; drop --live, or turn the check back on")
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	// Before the probes: a model whose key is a reference is asked with that key, so the sources have to be open, and a pipeline they cannot vouch for is refused before anything is dialled.
	resolver, secretProblems, found, err := v.secretProblems(ctx, cfg)
	if err != nil {
		return nil, 0, err
	}

	ctx = pipeline.WithProbeSecrets(pipeline.WithSecrets(ctx, resolver), resolvePipelineName(v.Pipeline, v.Name))

	var problems []config.Problem

	if v.Job != "" {
		job, err := cfg.FindJob(v.Job)
		if err != nil {
			return nil, 0, fmt.Errorf("cannot probe: %w", err)
		}

		problems = pipeline.Preflight(ctx, cfg, job)
	} else {
		names := make([]string, 0, len(cfg.Resources))
		for _, resource := range cfg.Resources {
			names = append(names, resource.Name)
		}

		problems = pipeline.PreflightPipeline(ctx, cfg, names)
	}

	return append(problems, secretProblems...), found, nil
}

// secretProblems resolves every reference the way the daemon would and says which lookup path answered, discarding the values. Resolution is lazy at run time, so this is the only way to learn that a login has lapsed, or that a name falls through to the shared folder, before a job is half paid for.
//
// Under --job, the job's own references: a trigger: true resource's poll is no job's, so it is left out the way the job's probe leaves out the other jobs' models.
func (v *ValidateCmd) secretProblems(ctx context.Context, cfg *config.Config) (*secret.Resolver, []config.Problem, int, error) {
	uses := cfg.SecretUses()
	if v.Job != "" {
		uses = slices.DeleteFunc(uses, func(use config.SecretUse) bool { return use.Job != v.Job })
	}

	// Opened before the references are counted: a path that cannot be read is wrong whether or not the pipeline has a reference yet.
	var resolver *secret.Resolver

	if v.Config != "" {
		var err error

		resolver, err = secretsFromConfigFile(v.Config)
		if err != nil {
			return nil, nil, 0, err
		}
	}

	if len(uses) == 0 {
		return resolver, nil, 0, nil
	}

	// Refused rather than skipped, for the reason the preflight refusal gives: an ok line that vouched for the pipeline having resolved none of its secrets would be vouching for nothing.
	if resolver == nil {
		return nil, nil, 0, fmt.Errorf("--live cannot vouch for %d secret reference(s) without the daemon's sources: pass --config with the file steps web --config reads", len(uses))
	}

	pipelineName := resolvePipelineName(v.Pipeline, v.Name)

	var problems []config.Problem

	found := map[config.SecretRef]bool{}

	for _, use := range uses {
		target := fmt.Sprintf("secret %s for %s", use.Ref, describeSecretUse(use))

		location, err := resolver.Locate(ctx, secret.Ref{Pipeline: pipelineName, Job: use.Job, Resource: use.Resource, Source: use.Ref.Source, Name: use.Ref.Name})
		if err != nil {
			problems = append(problems, config.Problem{Target: target, Detail: err.Error()})

			continue
		}

		found[use.Ref] = true

		fmt.Printf("%s is at %s\n", target, location)
	}

	return resolver, problems, len(found), nil
}

func describeSecretUse(use config.SecretUse) string {
	switch {
	case use.Polled && use.Resource == "":
		return "an mcp server when polled"
	case use.Polled:
		return fmt.Sprintf("resource %q when polled", use.Resource)
	case use.Resource != "":
		return fmt.Sprintf("resource %q in job %q", use.Resource, use.Job)
	default:
		return fmt.Sprintf("job %q", use.Job)
	}
}

// liveNote says which depth the ok line is vouching for, since the two read
// identically otherwise and only one of them talked to anything.
func liveNote(live bool, secrets int) string {
	if !live {
		return ""
	}

	note := " — every model and MCP server responded"
	if secrets > 0 {
		note += fmt.Sprintf(", %d secret(s) found", secrets)
	}

	return note
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
