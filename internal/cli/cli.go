// Package cli implements steps' command-line grammar and every command
// behind it: check discovers resource versions, get fetches one via a
// rendered shell command, and task runs a plan step's command. `run`
// executes one job once; `web` serves the UI, polls trigger: true resources
// and auto-runs every job a changed resource affects.
//
// It lives below main rather than in it so the end-to-end suite in ./e2e can
// drive the whole stack through Run, which is the only entry point that
// spans it.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"
	"github.com/lmittmann/tint"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/trigger"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
)

// CLI is the pipeline runner's command-line grammar, parsed by kong. Run is
// default:"withargs" so today's flat invocation (steps pipeline.yml --job x)
// keeps working unchanged, routed to it implicitly. LogLevel is a global flag
// (available to every subcommand, not just Run) rather than living on
// RunCmd/TestCmd individually, since it configures the process-wide slog
// default logger before any subcommand's Run method executes — see
// InitLogging.
type CLI struct {
	LogLevel  string           `default:"info"                          enum:"debug,info,warn,error"                                             env:"STEPS_LOG_LEVEL"        help:"log verbosity: debug, info, warn, or error"`
	Version   kong.VersionFlag `help:"print the steps version and exit" name:"version"`
	Run       RunCmd           `cmd:""                                  default:"withargs"                                                       help:"run a single job once"`
	Test      TestCmd          `cmd:""                                  help:"run every job (force) and verify assert directives"`
	Validate  ValidateCmd      `cmd:""                                  help:"check a pipeline for errors without running anything"`
	Runs      RunsCmd          `cmd:""                                  help:"show what past runs recorded"`
	Plan      PlanCmd          `cmd:""                                  help:"show which steps a run would execute or skip"`
	MCP       MCPCmd           `cmd:""                                  help:"inspect or authorize a pipeline's mcp_servers: entries"`
	Jobs      JobsCmd          `cmd:""                                  help:"jobs the circuit breaker has paused, and taking one out of it"`
	Approvals ApprovalsCmd     `cmd:""                                  help:"approval: steps waiting for a decision, and deciding them"`
	Questions QuestionsCmd     `cmd:""                                  help:"ask_user questions waiting for an answer, and answering them"`
	Web       WebCmd           `cmd:""                                  help:"serve the UI, poll trigger: true resources, and run affected jobs"`
	Docs      DocsCmd          `cmd:""                                  help:"read the docs in the terminal (no page name lists them)"`
	// Last, and hidden: see ShimCmd. Placing it here keeps the help ordering
	// of the real commands untouched.
	Shim ShimCmd `cmd:"" hidden:"" name:"_shim"`
}

// BuildVersion is the version string steps --version prints. Overridden at
// build time via -ldflags "-X github.com/jtarchie/steps/internal/cli.BuildVersion=...";
// "dev" covers `go run`/unversioned `go build` invocations.
var BuildVersion = "dev"

// RunCmd runs a single job's plan once, exactly as steps has always done.
type RunCmd struct {
	StateFlags   `embed:""`
	VarFlags     `embed:""`
	ExecFlags    `embed:""`
	HistoryFlags `embed:""`
	Pipeline     string            `arg:""                                                                 help:"path to the pipeline YAML file"`
	Job          string            `help:"job name to run (defaults to the pipeline's only job)"`
	Pin          map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                name:"pin"`
	Force        bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	Resume       string            `help:"continue a failed run from the step that failed"                 name:"resume"`
	Replay       string            `help:"fork a recorded run and re-run it from --from onward"            name:"replay"`
	From         string            `help:"with --replay, the step name to re-run from"                     name:"from"`
}

// applyContinuation handles the flags that point this invocation at a previous
// run, and reports which job to run.
//
// --resume resolves its job here; --replay only resolves its NAME here,
// because preparing it needs the job's plan to turn --from into a position —
// see applyReplay, which runs after selectJob.
func (r *RunCmd) applyContinuation(
	ctx context.Context, st *store.Store, provider workspace.Provider, jobName string,
) (context.Context, string, error) {
	if r.Resume != "" && r.Replay != "" {
		return ctx, "", errors.New("--resume and --replay cannot be combined: one continues a failed run in place, the other forks a recorded one from a step you name")
	}

	var err error

	if r.Resume != "" {
		ctx, jobName, err = applyResume(ctx, st, provider, r.Resume, jobName)
		if err != nil {
			return ctx, "", err
		}
	}

	if r.Replay != "" && jobName == "" {
		jobName, err = pipeline.ResumeJobName(ctx, st, r.Replay)
		if err != nil {
			return ctx, "", fmt.Errorf("could not replay: %w", err)
		}
	}

	return ctx, jobName, nil
}

// applyReplay forks a recorded run once the job is known.
func (r *RunCmd) applyReplay(
	ctx context.Context, st *store.Store, provider workspace.Provider, cfg *config.Config, job *config.Job,
) (context.Context, error) {
	if r.Replay == "" {
		return ctx, nil
	}

	if r.From == "" {
		return ctx, errors.New("--replay needs --from <step>: a replay re-runs from a step you name, and without one it would just re-run the whole plan")
	}

	ctx, _, err := pipeline.PrepareReplay(ctx, st, provider, r.Replay, r.From, cfg, job)
	if err != nil {
		return ctx, fmt.Errorf("could not replay: %w", err)
	}

	return ctx, nil
}

// Run loads the pipeline, selects a job, and runs it once via
// pipeline.RunJob.
func (r *RunCmd) Run() error {
	cfg, err := r.Load(r.Pipeline, resolvePipelineName(r.Pipeline, r.Name))
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, r.Pipeline, r.StateFlags, r.ExecFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	r.HistoryFlags.Apply(cfg)

	ctx, err = r.ExecFlags.Apply(ctx)
	if err != nil {
		return err
	}

	jobName := r.Job

	ctx, jobName, err = r.applyContinuation(ctx, st, provider, jobName)
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, jobName)
	if err != nil {
		return err
	}

	ctx, err = r.applyReplay(ctx, st, provider, cfg, job)
	if err != nil {
		return err
	}

	slog.Info("pipeline.run", "pipeline", r.Pipeline, "job", job.Name)

	runErr := pipeline.RunJob(ctx, cfg, job, r.Pin, provider, st, r.Force)

	slog.Info("pipeline.done", "pipeline", r.Pipeline, "job", job.Name, "error", runErr)

	// A successful manual run clears the watch circuit breaker: running the
	// job by hand is the natural way to confirm a fix, and requiring a
	// separate resume afterwards would be a step nobody remembers.
	if runErr == nil {
		_ = st.ResetJobFailures(context.WithoutCancel(ctx), job.Name)
	}

	return wrapRunErr(runErr)
}

// TestCmd runs every job in the pipeline (force, so nothing is skipped and the
// recorded execution order is deterministic) and verifies its assert:
// directives — each job's own assert.execution is checked inside RunJob, and a
// top-level assert.execution of job names is checked here. It's the entry
// point for a self-verifying fixture — every runnable example in docs/*.md
// is one (see docs_test.go).
type TestCmd struct {
	StateFlags `embed:""`
	VarFlags   `embed:""`
	ExecFlags  `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
}

// Run loads the pipeline, runs every job (force), and reports pass/fail per
// job plus the pipeline-level assert.execution. It returns a non-nil error if
// any job failed or the pipeline assert mismatched, so the process exits
// non-zero.
func (t *TestCmd) Run() error {
	cfg, err := t.Load(t.Pipeline, resolvePipelineName(t.Pipeline, t.Name))
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, t.Pipeline, t.StateFlags, t.ExecFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx, err = t.Apply(ctx)
	if err != nil {
		return err
	}

	var (
		executed []string
		failures []string
	)

	slog.Info("pipeline.test", "pipeline", t.Pipeline, "jobs", len(cfg.Jobs))

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]
		executed = append(executed, job.Name)

		jobErr := pipeline.RunJob(ctx, cfg, job, nil, provider, st, true)
		if jobErr != nil {
			fmt.Printf("FAIL %s: %v\n", job.Name, jobErr)

			// The REASON, not just the name. This error is what a caller
			// sees — a script, a CI step, or the mutation suite asking
			// which assertion caught a mutant — and "1 job(s) failed:
			// [build]" sends every one of them back to scrape stdout.
			failures = append(failures, fmt.Sprintf("  %s: %v", job.Name, jobErr))

			continue
		}

		fmt.Printf("PASS %s\n", job.Name)
	}

	slog.Info("pipeline.test.done", "pipeline", t.Pipeline, "jobs", len(executed), "failed", len(failures))

	if cfg.Assert != nil && len(cfg.Assert.Execution) > 0 && !slices.Equal(cfg.Assert.Execution, executed) {
		return fmt.Errorf("pipeline assert.execution mismatch:\n  want: %v\n  got:  %v", cfg.Assert.Execution, executed)
	}

	if len(failures) > 0 {
		return fmt.Errorf("test: %d job(s) failed:\n%s", len(failures), strings.Join(failures, "\n"))
	}

	fmt.Printf("%d/%d passed\n", len(executed), len(executed))

	return nil
}

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
}

// Run loads the pipeline (which runs every config-level validator) and then
// checks artifact flow for each job, joining the failures so one invocation
// reports everything wrong with the file.
func (v *ValidateCmd) Run() error {
	err := v.checkDepth()
	if err != nil {
		return err
	}

	cfg, err := v.Load(v.Pipeline, PipelineName(v.Pipeline))
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

// PlanCmd shows which steps a run would execute and which it would skip,
// without executing any of them.
//
// It is a distinct verb rather than a --dry-run flag on `run`: previewing is
// a read, and a flag on the run command reads as "and then run it".
//
// The planner already computes this and acts on it immediately, so finding
// out what a run would skip meant starting one — the wrong trade when the
// question is "is my cache in the state I think it is?".
type PlanCmd struct {
	StateFlags `embed:""`
	VarFlags   `embed:""`
	Pipeline   string            `arg:""                                                   help:"path to the pipeline YAML file"`
	Job        string            `help:"job to plan (defaults to the pipeline's only job)"`
	Pin        map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"  name:"pin"`
	// Worker alone of ExecFlags: planning runs a tagged resource's check where
	// a run would, and nothing else of a run.
	Worker map[string]string `help:"map a resource tag to a worker, e.g. --worker vpc=ssh://jt@box (repeatable)" name:"worker"`
}

// Run loads the pipeline, plans the selected job, and prints one line per
// step. Resource check commands run (planning has always resolved get
// versions), but no step executes and nothing is recorded.
func (p *PlanCmd) Run() error {
	cfg, err := p.Load(p.Pipeline, resolvePipelineName(p.Pipeline, p.Name))
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, p.Job)
	if err != nil {
		return err
	}

	st, err := store.OpenStore(StatePath(p.Pipeline, p.State), resolvePipelineName(p.Pipeline, p.Name))
	if err != nil {
		return fmt.Errorf("could not open state store: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx, err = pipeline.WithWorkers(ctx, p.Worker)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	rows, err := pipeline.Explain(ctx, cfg, job, p.Pin, st)
	if err != nil {
		return fmt.Errorf("could not plan job: %w", err)
	}

	if len(rows) == 0 {
		fmt.Printf("job %q plans no steps\n", job.Name)

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "ACTION\tSTEP\tHASH\tWHY")

	skips := 0

	for _, row := range rows {
		action := "run"
		if row.WouldSkip {
			action = "skip"
			skips++
		}

		_, _ = fmt.Fprintf(writer, "%s\t%s %s\t%s\t%s\n", action, row.Kind, row.Name, row.ShortHash, row.Reason)
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	fmt.Printf("\n%d step(s): %d would run, %d cached\n", len(rows), len(rows)-skips, skips)

	return nil
}

// RunsCmd is what past runs recorded, in five views.
//
// The store has always written all of this and offered no way to read it: the
// only route to "why did my last run fail" was opening .steps/state.db in
// sqlite and knowing the schema, which the vendored pure-Go driver means may
// not even be installed.
//
// Subcommands rather than a flag switch, and the difference is not
// cosmetic: each view differs in what it needs NAMED. `list` reads one
// pipeline or every pipeline in a --state file; the other four are questions
// about one pipeline and cannot be anything else — a trigger queue belongs to
// a pipeline, a step's job name means nothing without one, and --run already
// refuses an id belonging to a neighbour in the same file. As flags on one
// command that distinction was a runtime table of which combinations to
// refuse; as subcommands it is the grammar, and kong enforces it.
type RunsCmd struct {
	List  RunsListCmd  `cmd:"" default:"withargs"                                   help:"runs, newest first"`
	Steps RunsStepsCmd `cmd:"" help:"individual steps, with what each one recorded"`
	Queue RunsQueueCmd `cmd:"" help:"what the trigger loop has queued"`
	Cost  RunsCostCmd  `cmd:"" help:"what a pipeline's agent steps spent"`
	Where RunsWhereCmd `cmd:"" help:"the machines a run's placed steps ran on"`
}

// RunsListCmd is the default view: runs, newest first — and the one
// view that answers for a whole state file when no pipeline is named.
type RunsListCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""                            help:"path to the pipeline YAML file (omit, with --state, to read every pipeline in one file)" optional:""`
	Job        string `help:"only show runs of this job"`
	Limit      int    `default:"20"                      help:"maximum number of rows to show"`
}

// Run prints one pipeline's job runs, or every pipeline's in a shared file.
func (r *RunsListCmd) Run() error {
	// No pipeline named is the cross-pipeline question, which only a --state
	// file can hold: the default path is derived FROM a pipeline, so without
	// one there is nothing to read.
	if r.Pipeline == "" {
		return r.runAcross()
	}

	if nothingRecorded(r.Pipeline, r.StateFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.Pipeline, r.StateFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printJobRuns(context.Background(), st)
}

// RunsStepsCmd is the per-step detail: what previous runs actually did.
type RunsStepsCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""                             help:"path to the pipeline YAML file"`
	Job        string `help:"only show steps of this job"`
	Limit      int    `default:"20"                       help:"maximum number of rows to show"`
}

// Run prints recorded steps, newest first.
func (r *RunsStepsCmd) Run() error {
	if nothingRecorded(r.Pipeline, r.StateFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.Pipeline, r.StateFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printSteps(context.Background(), st)
}

// RunsQueueCmd is what the trigger loop has queued and not yet run.
type RunsQueueCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""       help:"path to the pipeline YAML file"`
	Limit      int    `default:"20" help:"maximum number of rows to show"`
}

// Run prints the trigger queue.
func (r *RunsQueueCmd) Run() error {
	if nothingRecorded(r.Pipeline, r.StateFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.Pipeline, r.StateFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printQueue(context.Background(), st)
}

// RunsCostCmd is what agent steps spent: per run, or per step within one run.
//
// The run id is a positional rather than a --run flag, because naming a run
// IS choosing the deeper view. As a flag it had to imply --cost to mean
// anything, which is a flag that reads as configured while binding nothing.
type RunsCostCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""       help:"path to the pipeline YAML file"`
	RunID      string `arg:""       help:"break this one run down per step" optional:""`
	Limit      int    `default:"20" help:"maximum number of rows to show"`
}

// Run prints per-run totals, or one run's steps.
func (r *RunsCostCmd) Run() error {
	if nothingRecorded(r.Pipeline, r.StateFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.Pipeline, r.StateFlags)
	if err != nil {
		return err
	}
	defer done()

	ctx := context.Background()

	if r.RunID != "" {
		return r.printRunCost(ctx, st)
	}

	return r.printCostTotals(ctx, st)
}

// RunsWhereCmd is which machines a run's placed steps ran on.
type RunsWhereCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""                                 help:"path to the pipeline YAML file"`
	RunID      string `arg:""                                 help:"the run to report on (default: the newest)" optional:""`
	Job        string `help:"take the newest run of this job"`
}

// Run prints one run's placements.
func (r *RunsWhereCmd) Run() error {
	if nothingRecorded(r.Pipeline, r.StateFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.Pipeline, r.StateFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printPlacements(context.Background(), st)
}

// nothingRecorded reports — and says — that a pipeline has no state file yet.
//
// Asked BEFORE opening, so asking about history never creates the database it
// is asking about: a read command that left a .steps/ behind would be a
// surprising thing for `steps runs` to do on a fresh checkout. It is a
// separate question from opening rather than a third return value, because a
// helper that answers "here is the store" and "there is no store" through one
// signature hands every caller a nil it must remember to check — which is
// exactly what a sixth `runs` subcommand written by copying the other five
// would forget.
func nothingRecorded(pipelinePath string, flags StateFlags, answer string) bool {
	path := StatePath(pipelinePath, flags.State)

	_, err := os.Stat(path)
	if err != nil {
		fmt.Println(answer)

		return true
	}

	// A file with no schema in it is the same answer as no file: a writer
	// creates the database before it fills it in, so a reader arriving in
	// that window must not report the operator's brand new database as one
	// written by a different version of steps.
	if store.HasNothingRecorded(path) {
		fmt.Println(answer)

		return true
	}

	return false
}

// noRunsYet is the sentence every `steps runs` view says when the pipeline has
// no state file.
func noRunsYet(pipelinePath string) string {
	return "no runs recorded yet for " + pipelinePath
}

// openRecorded opens a pipeline's recorded state for reading. It returns a
// usable store or an error, never both nil — call nothingRecorded first.
//
// OpenExisting, not OpenStore: asking must not register the pipeline it is
// asking about — see store.OpenExisting.
func openRecorded(pipelinePath string, flags StateFlags) (*store.Store, func(), error) {
	path := StatePath(pipelinePath, flags.State)

	st, err := store.OpenExisting(path, resolvePipelineName(pipelinePath, flags.Name))
	if err != nil {
		return nil, nil, fmt.Errorf("could not open state store: %w", err)
	}

	return st, func() { _ = st.Close() }, nil
}

// runAcross reports on every pipeline in one state file: what it holds, and
// the newest runs across all of it.
//
// This is the CLI's answer to the question --state created. `steps runs` is
// otherwise scoped by the pipeline it is handed, so a file with three
// pipelines in it took three invocations to read and gave no interleaving at
// all — the web root has answered this since it learned to serve several
// pipelines, and it reads through the same store.Reader.
//
// The view is chosen by whether a pipeline was NAMED, not by how many the
// file turns out to hold: a one-pipeline file still prints the pipeline
// column here, so a script that reads this output gets the same columns
// whatever the file grows into.
func (r *RunsListCmd) runAcross() error {
	if r.State == "" {
		return errors.New("steps runs list needs a pipeline to read, or --state <file> to read every pipeline in one state database")
	}

	// The only flag left that a whole file cannot answer: RecentRuns spans
	// pipelines and does not filter by job, and two pipelines calling a job
	// `build` are not one job. Refused rather than silently ignored.
	if r.Job != "" {
		return fmt.Errorf("--job asks about one pipeline: run `steps runs list <pipeline> --job %s --state %s`", r.Job, r.State)
	}

	// Stat first, for the same reason the scoped path does: asking about
	// history must not create the database it is asking about.
	_, err := os.Stat(r.State)
	if err != nil {
		fmt.Printf("no state database at %s\n", r.State)

		return nil
	}

	reader, err := store.OpenReader(r.State)
	if errors.Is(err, store.ErrNoState) {
		// Created but not yet filled in — a writer is mid-first-open. Nothing
		// is recorded, which is an answer, not a file to delete.
		fmt.Printf("no pipelines recorded in %s\n", r.State)

		return nil
	}

	if err != nil {
		return fmt.Errorf("could not open state store: %w", err)
	}
	defer func() { _ = reader.Close() }()

	ctx := context.Background()

	pipelines, err := reader.Pipelines(ctx)
	if err != nil {
		return fmt.Errorf("could not read pipelines: %w", err)
	}

	if len(pipelines) == 0 {
		fmt.Printf("no pipelines recorded in %s\n", r.State)

		return nil
	}

	err = printPipelines(pipelines)
	if err != nil {
		return err
	}

	return r.printRunsAcross(ctx, reader, pipelines)
}

// printPipelines lists what the file holds. A name alone does not say which
// YAML is behind it — the name is the identity and the path is only what a
// human reads back — so both are printed.
func printPipelines(pipelines []store.PipelineRow) error {
	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "PIPELINE\tPATH")

	for _, pipeline := range pipelines {
		// Empty when no command that LOADED the YAML has opened this
		// pipeline yet, which a file written by an older build can hold. A
		// dash rather than a blank column, so the row reads as unanswered
		// rather than as a pipeline living at "".
		path := pipeline.Path
		if path == "" {
			path = "-"
		}

		_, _ = fmt.Fprintf(writer, "%s\t%s\n", pipeline.Name, path)
	}

	err := flush(writer)
	if err != nil {
		return err
	}

	fmt.Println()

	return nil
}

// printRunsAcross prints the interleaved feed, newest first.
//
// Every pipeline in the FILE is named, which is where this parts company with
// the web root: that one names only the pipelines the process serves, because
// a row it cannot link anywhere is worse than a missing one. The CLI serves
// nothing and links nowhere, so a run recorded by a pipeline whose YAML has
// since moved is still an answer to what ran.
//
// Runs rather than job_runs, which is what the scoped default reads: job_runs
// is the merkle cache index, keyed and upserted by content hash, so its
// timestamp means "last time this content ran" rather than "a build
// happened". Across pipelines the useful row is a real run with an id, which
// is the handle for going and asking that pipeline about it.
func (r *RunsListCmd) printRunsAcross(ctx context.Context, reader *store.Reader, pipelines []store.PipelineRow) error {
	names := make([]string, 0, len(pipelines))
	for _, pipeline := range pipelines {
		names = append(names, pipeline.Name)
	}

	rows, err := reader.RecentRuns(ctx, names, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read runs: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no runs recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tPIPELINE\tJOB\tSTATUS\tRUN\tCONFIG")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			formatWhen(row.StartedAt), row.Pipeline, row.JobName, row.Status, row.ID, shortConfig(row.ConfigSHA))
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	fmt.Printf("\nbreak one down with: steps runs cost <pipeline> <run> --state %s\n", r.State)

	return nil
}

// printJobRuns lists what actually ran, newest first.
//
// The runs table, not job_runs, which is what this view read until it was
// caught: job_runs is the merkle CACHE index, and recordChainSucceeded skips
// a chain containing a put or an agent because such a chain is never
// skippable — so the default history view of an agent pipeline recorded every
// failure and no success, and read as all-red or empty after a run that
// worked. It is also upserted by content hash, so twenty forced re-runs were
// one row.
//
// This is the same source the web UI and the cross-pipeline view read, which
// is the other half of the fix: one command that meant two different tables
// depending on whether a pipeline was named is a command nobody can reason
// about. The error text moves one command along, to `steps runs steps`, which
// reports it per step — where the answer to "why did it fail" actually is.
func (r *RunsListCmd) printJobRuns(ctx context.Context, st *store.Store) error {
	rows, err := st.ListRuns(ctx, r.Job, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read runs: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no runs recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tJOB\tSTATUS\tRUN\tCONFIG")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			formatWhen(row.StartedAt), row.JobName, row.Status, row.ID, shortConfig(row.ConfigSHA))
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	fmt.Printf("\nwhy a step did what it did: steps runs steps %s%s\n", r.Pipeline, stateNote(r.State))

	return nil
}

// RecordRevision writes down WHICH configuration the runs opened against this
// handle will have executed.
//
// Called from the same funnel as SetSourcePath, and for the same reason: it
// is the only place holding both the configuration that was parsed and the
// handle its runs are recorded against. A command that only READS history
// never comes through there and records no revision, which is right — it
// resolved no configuration.
func RecordRevision(ctx context.Context, st *store.Store, cfg *config.Config) error {
	if !cfg.Revision.Recorded() {
		return nil
	}

	err := st.RecordRevision(ctx, cfg.Revision.SHA, cfg.Revision.Source)
	if err != nil {
		return fmt.Errorf("could not record the pipeline's configuration: %w", err)
	}

	return nil
}

// shortConfig abbreviates a configuration hash for a column a human scans.
//
// The question this column answers is "did the pipeline change between these
// two runs", which is a comparison and not an identifier — so it is prefixed
// to something that fits beside the other four rather than printed whole. A
// run that recorded no configuration says so rather than printing a blank
// cell, which reads as a column that failed to fill in.
func shortConfig(sha string) string {
	const shown = 12

	if sha == "" {
		return "-"
	}

	if len(sha) <= shown {
		return sha
	}

	return sha[:shown]
}

func (r *RunsStepsCmd) printSteps(ctx context.Context, st *store.Store) error {
	rows, err := st.ListNodes(ctx, r.Job, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read steps: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no steps recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tJOB\tSTEP\tSTATUS\tERROR")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s %s\t%s\t%s\n",
			formatWhen(row.CreatedAt), row.JobName, row.Kind, row.Resource, row.Status, firstLine(row.Error))
	}

	return flush(writer)
}

func (r *RunsQueueCmd) printQueue(ctx context.Context, st *store.Store) error {
	rows, err := st.ListTriggerQueue(ctx, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read the trigger queue: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("trigger queue is empty")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "ENQUEUED\tJOB\tSTATUS\tREASON\tERROR")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			formatWhen(row.EnqueuedAt), row.JobName, row.Status, row.Reason, firstLine(row.Error))
	}

	return flush(writer)
}

// newTabWriter returns the aligned-column writer every runs view prints
// through.
func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func flush(writer *tabwriter.Writer) error {
	err := writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write output: %w", err)
	}

	return nil
}

// formatWhen renders a timestamp in local time, or "-" when unset.
func formatWhen(when time.Time) string {
	if when.IsZero() {
		return "-"
	}

	return when.Local().Format("2006-01-02 15:04:05")
}

// firstLine truncates an error to its first line and a readable width, since
// these are columns in a table — the full text is in the run's own output.
func firstLine(text string) string {
	if text == "" {
		return "-"
	}

	line, _, _ := strings.Cut(text, "\n")

	const maxWidth = 70
	if len(line) > maxWidth {
		return line[:maxWidth-1] + "…"
	}

	return line
}

// MCPCmd groups the two mcp_servers:-related subcommands: `tools` (list a
// server's tools — works for any auth type, and is the discovery/preflight
// step a pipeline author runs before writing a tool reference or a
// resource type's mcp: block) and `login` (the only interactive,
// state-writing command in this group — see internal/mcp/login.go). Neither
// `run` nor `watch` ever prompts; a headless process that hits an
// unauthorized oauth server just surfaces the actionable error naming this
// login command.
type MCPCmd struct {
	List  MCPListCmd  `cmd:"" help:"list the pipeline's mcp servers and whether each one answers"`
	Tools MCPToolsCmd `cmd:"" help:"list an mcp server's tools and their argument schemas"`
	Login MCPLoginCmd `cmd:"" help:"interactively authorize an oauth-configured mcp server"`
}

// MCPListCmd is the inventory: every mcp_servers: entry, how it connects, who
// consumes it, and — unless --offline — whether it answers right now.
//
// `mcp tools` answers "what can this one server do", which presumes you
// already know which servers exist and which are working. This is the step
// before that, and the reason it probes by default: a server is configured in
// YAML but broken on this machine (binary not on PATH, token never obtained,
// endpoint moved), and the file alone cannot tell you which.
//
// It reports rather than gates — a server that does not answer is a row with
// an ✗, not a non-zero exit. `steps validate --live` is the command that fails.
type MCPListCmd struct {
	Pipeline string `arg:""                                                            help:"path to the pipeline YAML file"`
	Offline  bool   `help:"list what the file declares without connecting to anything" name:"offline"`
}

// Run prints one row per configured server.
func (m *MCPListCmd) Run() error {
	cfg, err := config.LoadConfig(m.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	if len(cfg.MCPServers) == 0 {
		fmt.Printf("no mcp_servers: entries in %s\n", m.Pipeline)

		return nil
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	// Sized up front so a row's status cell is indexable whether or not
	// anything was probed — the offline path prints no STATUS column at all.
	statuses := make([]string, len(cfg.MCPServers))

	var probeErr error

	if !m.Offline {
		statuses, probeErr = probeMCPServers(ctx, cfg)
	}

	writer := newTabWriter()

	header := "NAME\tTRANSPORT\tTARGET\tAUTH\tUSED BY"
	if !m.Offline {
		header += "\tSTATUS"
	}

	_, _ = fmt.Fprintln(writer, header)

	for i, srv := range cfg.MCPServers {
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s",
			srv.Name, mcpTransport(srv), mcpTarget(srv), mcpAuth(srv), mcpUsers(cfg, srv.Name))

		if !m.Offline {
			row += "\t" + statuses[i]
		}

		_, _ = fmt.Fprintln(writer, row)
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	// An interrupted probe leaves rows reading "✗ context canceled", which is
	// indistinguishable from a pipeline whose every server is down — so the
	// exit status has to be the thing that separates them (130, not 0).
	if probeErr != nil {
		return fmt.Errorf("mcp list: %w", probeErr)
	}

	return nil
}

// probeMCPServers connects to every server and reports what it found, one
// status per server in configuration order, plus the context's own error so an
// interrupted listing is not mistaken for a listing of broken servers.
//
// Concurrently, because the failure this command exists to surface is a server
// that does not answer — and doing that serially means the slowest possible
// listing is the one with the most broken servers in it, each waiting out its
// own timeout in turn.
func probeMCPServers(ctx context.Context, cfg *config.Config) ([]string, error) {
	var settings *config.Preflight
	if cfg.Defaults != nil {
		settings = cfg.Defaults.Preflight
	}

	timeout := settings.ProbeTimeout()
	statuses := make([]string, len(cfg.MCPServers))

	var wait sync.WaitGroup

	for i, srv := range cfg.MCPServers {
		if skip := mcpUnprobable(srv); skip != "" {
			statuses[i] = skip

			continue
		}

		wait.Add(1)

		go func() {
			defer wait.Done()

			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			tools, err := stepsmcp.ListServerTools(probeCtx, srv)
			if err != nil {
				statuses[i] = "✗ " + mcpStatusReason(srv.Name, err)

				return
			}

			statuses[i] = fmt.Sprintf("✓ %d %s", len(tools), pluralize(len(tools), "tool"))
		}()
	}

	wait.Wait()

	return statuses, ctx.Err() //nolint:wrapcheck // the caller names the command; a bare context.Canceled is what outcome.ExitCode reads
}

// mcpUnprobable reports the status for a server this command cannot honestly
// probe, or "" for one it can.
//
// A relative cwd: is resolved against the working directory of the agent step
// whose tools are being built (see config.WithResolvedMCPCwd) — a build
// workspace that exists only during a run. Spawning it from wherever the
// operator happens to have run `steps mcp list` would chdir somewhere else
// entirely, and report a server that works perfectly in a run as broken.
func mcpUnprobable(srv config.MCPServer) string {
	if srv.IsStdio() && srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
		return fmt.Sprintf("· not probed (cwd: %s resolves per step)", srv.Cwd)
	}

	return ""
}

// mcpStatusReason renders a probe failure as a table cell. It drops the copies
// of the server's name the error carries — the row's first column is already
// that name — because the actionable part of these messages ("run `steps mcp
// login` …", "$TOKEN is not set") is at the END, and is what the column's width
// budget should be spent on.
func mcpStatusReason(name string, err error) string {
	reason, _, _ := strings.Cut(err.Error(), "\n")

	for _, noise := range []string{
		"mcp: ",
		fmt.Sprintf("connect to %q: ", name),
		fmt.Sprintf("mcp server %q: ", name),
		fmt.Sprintf("mcp server %q ", name),
	} {
		reason = strings.TrimPrefix(reason, noise)
	}

	return elideMiddle(reason, maxStatusWidth)
}

// maxStatusWidth is how wide the STATUS cell may get before it is elided —
// the same budget firstLine spends on the error columns of the other tables.
const maxStatusWidth = 70

// elideMiddle drops the middle of an over-long reason rather than its tail.
//
// Both ends carry meaning and neither survives the other's loss: a dial
// failure names what was attempted first ("Post https://…") and how it went
// last ("connection refused"), and truncating from the right — which is what
// every other column here does, where the head is the whole content — keeps
// only the URL nobody was asking about. Runes, not bytes: half a rune in a
// tabwriter cell miscounts the column as well as printing as garbage.
func elideMiddle(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}

	head := width / 3

	return string(runes[:head]) + "…" + string(runes[len(runes)-(width-head-1):])
}

func mcpTransport(srv config.MCPServer) string {
	if srv.IsStdio() {
		return "stdio"
	}

	return "http"
}

// mcpTarget renders what the server actually is: the endpoint for HTTP, the
// argv (plus any pinned working directory) for stdio.
func mcpTarget(srv config.MCPServer) string {
	if !srv.IsStdio() {
		return srv.Endpoint
	}

	target := strings.Join(append([]string{srv.Command}, srv.Args...), " ")
	if srv.Cwd != "" {
		target += fmt.Sprintf(" (cwd: %s)", srv.Cwd)
	}

	return target
}

// mcpAuth names the auth type and, for bearer, the environment variable the
// credential is read from — the thing to go check when it is the credential
// that is missing. Never the value.
func mcpAuth(srv config.MCPServer) string {
	if srv.Auth.Type == "" || srv.Auth.Type == "none" {
		return "none"
	}

	if srv.Auth.APIKeyEnv != "" {
		return srv.Auth.Type + " $" + srv.Auth.APIKeyEnv
	}

	return srv.Auth.Type
}

func mcpUsers(cfg *config.Config, name string) string {
	users := cfg.MCPServerUsers(name)
	if len(users) == 0 {
		return "(unused)"
	}

	return strings.Join(users, ", ")
}

// pluralize adds the English plural s, so a count reads as a phrase.
func pluralize(count int, noun string) string {
	if count == 1 {
		return noun
	}

	return noun + "s"
}

// MCPToolsCmd lists the tools a configured mcp_servers: entry exposes.
type MCPToolsCmd struct {
	Pipeline string `arg:"" help:"path to the pipeline YAML file"`
	Server   string `arg:"" help:"mcp_servers: entry name"`
}

// Run loads the pipeline, resolves the named server, connects (per its
// configured auth), and prints each of its tools' name, description, and
// argument schema. An unauthorized oauth server surfaces
// oauthTokenSource's own actionable "run steps mcp login" error, unchanged.
func (m *MCPToolsCmd) Run() error {
	cfg, err := config.LoadConfig(m.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	srv, err := cfg.FindMCPServer(m.Server)
	if err != nil {
		return fmt.Errorf("could not find mcp server: %w", err)
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	tools, err := stepsmcp.ListServerTools(ctx, *srv)
	if err != nil {
		return fmt.Errorf("could not list tools: %w", err)
	}

	printMCPTools(tools)

	return nil
}

// printMCPTools writes each tool's name, description, and argument schema
// to stdout in a simple, human-readable form.
func printMCPTools(tools []*sdkmcp.Tool) {
	if len(tools) == 0 {
		fmt.Println("(no tools)")

		return
	}

	for _, tool := range tools {
		fmt.Printf("%s\n", tool.Name)

		if tool.Description != "" {
			fmt.Printf("  %s\n", tool.Description)
		}

		schema, err := json.MarshalIndent(tool.InputSchema, "  ", "  ")
		if err == nil && len(schema) > 0 {
			fmt.Printf("  arguments: %s\n", schema)
		}

		fmt.Println()
	}
}

// MCPLoginCmd interactively authorizes an auth: {type: oauth} mcp_servers:
// entry — the only command in this CLI that opens a browser or blocks on
// user interaction outside a pipeline run.
type MCPLoginCmd struct {
	Pipeline string `arg:"" help:"path to the pipeline YAML file"`
	Server   string `arg:"" help:"mcp_servers: entry name to authorize"`
}

// Run resolves the named server (rejecting anything but auth: {type:
// oauth} — there is nothing to log in to otherwise) and runs the
// interactive authorization-code + PKCE flow, printing progress a human can
// follow.
func (m *MCPLoginCmd) Run() error {
	cfg, err := config.LoadConfig(m.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	srv, err := cfg.FindMCPServer(m.Server)
	if err != nil {
		return fmt.Errorf("could not find mcp server: %w", err)
	}

	if srv.Auth.Type != "oauth" {
		return fmt.Errorf("mcp server %q is not auth: {type: oauth}; nothing to log in to", m.Server)
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	fmt.Printf("→ opening browser to authorize %q…\n", m.Server)

	err = stepsmcp.Login(ctx, *srv, openBrowser)
	if err != nil {
		return fmt.Errorf("mcp login: %w", err)
	}

	path, err := stepsmcp.TokenPath(m.Server)
	if err != nil {
		return fmt.Errorf("mcp login: %w", err)
	}

	fmt.Printf("✓ authorized %q (token saved to %s)\n", m.Server, path)

	return nil
}

// openBrowser launches the OS's default browser at url. Its caller
// (internal/mcp's loopbackCallback.fetch) prints url to stdout regardless
// and reports any error this returns alongside it, so this only needs to
// cover the happy path per OS — a nil case (no known opener for GOOS) fails
// closed into "open the URL above" rather than guessing.
func openBrowser(url string) error {
	// A fire-and-forget subprocess launch (handing off to the OS's default-
	// app opener, which returns almost immediately) with nothing meaningful
	// to cancel — context.Background() rather than threading a caller ctx
	// through internal/mcp's open func(string) error callback type.
	ctx := context.Background()

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", url) //nolint:gosec // url is the authorization URL steps itself just built via oauth2.Config.AuthCodeURL, not attacker-influenced input
	case "linux":
		cmd = exec.CommandContext(ctx, "xdg-open", url) //nolint:gosec // same as above
	default:
		return fmt.Errorf("no known browser-open command for GOOS %q", runtime.GOOS)
	}

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	return nil
}

// DefaultLogLevel is what InitLogging runs at before the CLI's own
// --log-level/STEPS_LOG_LEVEL has been parsed (during kong construction
// itself, and as CLI.LogLevel's own default) — never debug, so a parse
// failure (or any other pre-parse code path) can't fall back to printing
// every subsequent command/output dump by accident.
const DefaultLogLevel = "info"

// parseLogLevel maps a --log-level/STEPS_LOG_LEVEL string to a slog.Level,
// falling back to slog.LevelInfo for anything unrecognized — reachable only
// if called before kong's own enum: validation on CLI.LogLevel runs (e.g.
// DefaultLogLevel's own value, or a hypothetical future caller), never from
// a successfully parsed CLI. A standalone function (rather than inlined
// into InitLogging) so it can be unit-tested without touching the global
// slog default logger, which every run() call in this package's test suite
// also mutates — asserting on slog.Default() itself would race against
// whichever other test's run() call happens to finish last.
func parseLogLevel(level string) slog.Level {
	parsed, ok := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}[level]
	if !ok {
		return slog.LevelInfo
	}

	return parsed
}

// InitLogging installs a slog handler on stderr as the default logger,
// separate from this tool's plain stdout progress lines ("get: prs
// (version: ...)", "task: review"), at the given level (see parseLogLevel).
// Debug is what previously ran unconditionally on every invocation: shell.go/
// docker.go log the full rendered command and complete captured stdout/
// stderr at that level, so any resource check/in/out command whose
// templated source: or output embeds a credential was written to stderr on
// every ordinary run, with no way to suppress it. Defaulting to info (see
// CLI.LogLevel) makes that opt-in, via --log-level debug or
// STEPS_LOG_LEVEL=debug, rather than the permanent default.
func InitLogging(level string) {
	slog.SetDefault(slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:     parseLogLevel(level),
		AddSource: true,
		NoColor:   wantNoColor(),
	})))
}

// wantNoColor reports whether log output should skip ANSI color: either
// stderr isn't a terminal (piped, redirected to a file, captured by a
// screen reader or log tool) or the operator opted out via NO_COLOR
// (https://no-color.org). Terminal detection is a stdlib character-device
// check (no isatty dependency needed) rather than an internal/shell-style
// pattern, since this is the one place in the process that cares whether
// its own stderr is a live terminal. Checked at every InitLogging call
// rather than cached, since tests re-invoke it against different stderr
// targets.
func wantNoColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}

	info, err := os.Stderr.Stat()
	if err != nil {
		return true
	}

	return info.Mode()&os.ModeCharDevice == 0
}

// Run parses args as the steps command line and executes the command they
// name. It is the whole CLI behind one call, which is what lets the
// end-to-end suite drive a real invocation in-process rather than spawning
// a binary.
func Run(args []string) error {
	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Description("run pipeline jobs, or serve and poll them with steps web"), kong.Vars{"version": BuildVersion})
	if err != nil {
		return fmt.Errorf("could not build CLI parser: %w", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return fmt.Errorf("could not parse flags: %w", err)
	}

	InitLogging(cli.LogLevel)

	slog.Debug("cli.parse", "args", args)

	return kctx.Run() //nolint:wrapcheck // the Run methods above already wrap their own errors via wrapRunErr
}

// setup opens the state store and builds/validates the workspace provider
// shared by the commands that run steps, returning a cleanup func that closes both
// (logging, not returning, any close error — mirroring the deferred
// close-error handling both commands used inline before this helper
// existed).
func setup(
	cfg *config.Config, pipelinePath string, flags StateFlags, exec ExecFlags,
) (*store.Store, workspace.Provider, func(), error) {
	name := resolvePipelineName(pipelinePath, flags.Name)

	// The identity the caller loaded the Config under has to be the identity
	// the store is opened under, because they are one identity: the store
	// scopes every row by it, the /p/<slug> route is it, and the Config
	// carries it to whatever else is keyed by pipeline (an agent pin). They
	// were computed independently and silently disagreed, which is the whole
	// of #94 — so a caller that resolves it differently, or forgets and takes
	// the file-name default while --name says otherwise, is told rather than
	// left to split.
	if cfg.Name != name {
		return nil, nil, nil, fmt.Errorf(
			"the pipeline was loaded as %q but its state is scoped to %q — these are one identity, so load it with resolvePipelineName(path, flags.Name)",
			cfg.Name, name)
	}

	st, err := store.OpenStore(StatePath(pipelinePath, flags.State), name)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not open state store: %w", err)
	}

	// This is the funnel every command that LOADS a pipeline comes through
	// (run, test, web, jobs, approvals), and the only place holding
	// both the YAML path and the handle to record it against.
	err = st.SetSourcePath(context.Background(), pipelinePath)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("could not record the pipeline's source path: %w", err)
	}

	err = RecordRevision(context.Background(), st, cfg)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, err
	}

	provider, err := workspace.NewProvider(cfg.Workspace, exec.KeepWorkspace)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("could not build workspace provider: %w", err)
	}

	err = provider.Validate()
	if err != nil {
		// The provider owns a temp root it created; only its Close removes
		// one. Closing the store alone left a steps-* directory behind on
		// every attempt, with nothing to reap it.
		_ = provider.Close()
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("workspace: %w", err)
	}

	err = attachArtifactStore(provider, st, exec.ArtifactStore)
	if err != nil {
		_ = provider.Close()
		_ = st.Close()

		return nil, nil, nil, err
	}

	cleanup := func() {
		closeErr := provider.Close()
		if closeErr != nil {
			slog.Error("workspace.close", "error", closeErr)
		}

		closeErr = st.Close()
		if closeErr != nil {
			slog.Error("store.close", "error", closeErr)
		}
	}

	return st, provider, cleanup, nil
}

// attachArtifactStore wires --artifact-store into the provider's step cache:
// the blob half from the URL, the index half from the state store the digests
// are truth in. A pipeline with no durable workspace.root: has no step cache
// to mirror — that half is warned about rather than refused, because the
// flag's other consumer, a placed step's data plane, works without one.
func attachArtifactStore(provider workspace.Provider, st *store.Store, raw string) error {
	if raw == "" {
		return nil
	}

	opts, err := blobstore.Parse(raw)
	if err != nil {
		return err //nolint:wrapcheck // blobstore's own errors name the URL and the rule it broke
	}

	blobs, err := blobstore.New(context.Background(), opts)
	if err != nil {
		return err //nolint:wrapcheck // as above
	}

	if !workspace.AttachArtifactStore(provider, blobs, st) {
		slog.Warn("artifact_store.no_step_cache",
			"store", raw,
			"why", "no durable workspace.root:, so cached step outputs are not mirrored; placed steps still use the store as their data plane")
	}

	return nil
}

// wrapRunErr adds context to a RunJob/Watch error without adding another
// branch to the caller, which is already at cyclop's per-function
// complexity budget.
func wrapRunErr(err error) error {
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

// StateFlags are the two flags every command that opens a state database
// carries: WHERE the database is, and WHAT this pipeline is called inside it.
//
// Embedded rather than repeated because they always travel together — a
// --state naming a shared file is exactly when --name starts to matter.
type StateFlags struct {
	State string            `help:"path to the sqlite state database (default: .steps/<pipeline>.db beside the YAML)"      name:"state"`
	Name  map[string]string `help:"name a pipeline inside the state db, e.g. --name infra=infra/pipeline.yml (repeatable)" name:"name"`
}

// VarFlags carry ((name)) substitutions into a pipeline load.
//
// Declared once and embedded rather than repeated, because seven commands
// take them and a copy that drifts is a flag that silently means something
// else on one verb. Load is part of the embed for the same reason: a command
// that takes these flags and loads the pipeline some other way has declared
// an input it does not read, which is the shape of bug this consolidation
// exists to make impossible.
type VarFlags struct {
	Var      map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..." name:"var"`
	VarsFile string            `help:"YAML file of pipeline vars"                          name:"vars-file"`
}

// Load reads the pipeline at path, under the identity name, with these vars
// applied.
//
// name is passed rather than derived from path because --name lives on
// StateFlags and this embed cannot see it: a caller resolves the identity once
// (resolvePipelineName) and hands the same string to the store and to here, so
// the two cannot disagree. Positional for the reason config.Load makes it so.
func (v VarFlags) Load(path string, name string) (*config.Config, error) {
	return loadWithVars(path, name, v.Var, v.VarsFile)
}

// Revision is WHICH configuration the file at path currently is, under these
// vars — the same hash Load would stamp, for the cost of a read.
//
// It is what the reload watcher asks once a second so that parsing, and every
// validator behind it, happens only when the answer has actually moved.
func (v VarFlags) Revision(path string, includes []string) (config.Revision, error) {
	vars, err := resolveVars(v.Var, v.VarsFile)
	if err != nil {
		return config.Revision{}, err
	}

	revision, err := config.FileRevision(path, vars, includes)
	if err != nil {
		return config.Revision{}, fmt.Errorf("could not load pipeline: %w", err)
	}

	return revision, nil
}

// ExecFlags shape how a command that RUNS steps executes them: which machines
// tags: resolve to, where cached outputs are mirrored, what a parked question
// is answered with, whether the workspace survives, and whether the pre-run
// health check happens at all.
//
// One embed rather than five, because they are one idea — what the operator
// asked for about this execution — and because Apply is then the single place
// that threads them, which no embedder can forget without failing to compile.
type ExecFlags struct {
	Worker        map[string]string `help:"map a step tag to a worker, e.g. --worker gpu=ssh://jt@box (repeatable)"                           name:"worker"`
	ArtifactStore string            `help:"mirror cached step outputs to a content-addressed store, e.g. --artifact-store s3://bucket/prefix" name:"artifact-store"`
	Answer        []string          `help:"answer an ask_user question in advance, e.g. --answer 'which bump=minor' (repeatable)"             name:"answer"`
	KeepWorkspace bool              `env:"STEPS_KEEP_WORKSPACE"                                                                               help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight   bool              `help:"skip the pre-run health check of the models and MCP servers a job needs"                           name:"no-preflight"`
}

// Apply folds these flags into the context every step below reads them from.
//
// Workers are PARSED here rather than at first use, so a typo in a worker URL
// is reported with everything else wrong with the invocation instead of
// mid-plan, when some step happens to reach for it.
func (e ExecFlags) Apply(ctx context.Context) (context.Context, error) {
	if e.NoPreflight {
		ctx = pipeline.WithoutPreflight(ctx)
	}

	ctx, err := pipeline.WithWorkers(ctx, e.Worker)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	ctx = pipeline.WithArtifactStore(ctx, e.ArtifactStore)
	ctx = pipeline.WithKeepWorkspace(ctx, e.KeepWorkspace)

	ctx, err = pipeline.WithAnswers(ctx, e.Answer)
	if err != nil {
		return nil, fmt.Errorf("could not read --answer: %w", err)
	}

	return ctx, nil
}

// HistoryFlags bound what a build leaves behind.
//
// Both limits belong on every command that records: a manual run writes the
// same nodes, events and transcripts a triggered one does. --version-history
// was once declared on `run` and threaded nowhere, which is why declaring and
// applying are the same embed now.
type HistoryFlags struct {
	VersionHistory int `help:"how many versions of each resource to remember (pipeline defaults.version_history wins)" name:"version-history"`
	RunHistory     int `help:"how many runs of each job to keep (pipeline defaults.run_history wins)"                  name:"run-history"`
}

// Apply lets the flags stand in for limits the pipeline did not set. The
// pipeline wins where it spoke: it is the thing under version control.
func (h HistoryFlags) Apply(cfg *config.Config) {
	applyHistoryFlags(cfg, h.VersionHistory, h.RunHistory)
}

// StatePath returns the sqlite database path for pipeline's persisted job
// state: under .steps/ beside the pipeline YAML, named for the FILE — unless
// --state names one, which is how several pipelines come to share a file.
//
// Per file BY DEFAULT, not per directory, and that default is load-bearing on
// its own. Two pipelines in one folder are two namespaces, and a `.steps/state.db`
// that merged them by accident of layout was a bug: one pipeline's version
// change could enqueue a job the other then claimed and ran. Sharing is now
// something an operator asks for, and the database keeps them apart when they
// do — every row is scoped to a pipelines row (see internal/store/schema.go).
//
// There is no migration, per this repo's no-migration rule: a database from an
// older schema is refused rather than upgraded.
func StatePath(pipeline, state string) string {
	if state != "" {
		return state
	}

	return filepath.Join(filepath.Dir(pipeline), ".steps", filepath.Base(pipeline)+".db")
}

// PipelineName is a pipeline's identity inside a state database: the YAML's
// base name without its extension.
//
// The same string web.Slugify and config.Slugify produce, and deliberately so
// — the UI's /p/<slug> route, the database's pipelines.name and the Config's
// own name are one identity, not three that have to be kept in agreement.
func PipelineName(pipeline string) string {
	return config.Slugify(pipeline)
}

// resolvePipelineName applies the --name overrides to one pipeline path.
//
// The map is keyed by NAME, matching how it is typed (--name infra=infra/ci.yml)
// and giving uniqueness for free: two paths cannot claim one name, because the
// second assignment would replace the first rather than collide silently.
// Nothing matching means the default — the base name — which is what makes the
// flag needed only when a shared --state has two pipeline.yml in it.
func resolvePipelineName(pipeline string, names map[string]string) string {
	want, err := filepath.Abs(pipeline)
	if err != nil {
		want = filepath.Clean(pipeline)
	}

	for name, path := range names {
		got, err := filepath.Abs(path)
		if err != nil {
			got = filepath.Clean(path)
		}

		if got == want {
			return name
		}
	}

	return PipelineName(pipeline)
}

// selectJob resolves which job to run: the explicit name if given, or the
// pipeline's only job if there's exactly one and none was given.
func selectJob(cfg *config.Config, name string) (*config.Job, error) {
	if name == "" {
		if len(cfg.Jobs) != 1 {
			return nil, fmt.Errorf("--job is required when the pipeline has more than one job (available: %v)", cfg.JobNames())
		}

		name = cfg.Jobs[0].Name
		slog.Debug("cli.select_job", "job", name, "reason", "only job in pipeline")
	}

	job, err := cfg.FindJob(name)
	if err != nil {
		return nil, fmt.Errorf("could not select job: %w", err)
	}

	return job, nil
}

// withSignalCancel derives a context from parent that is canceled on
// SIGINT/SIGTERM, and returns it along with its cancel func.
func withSignalCancel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case sig := <-sigs:
			slog.Warn("signal.received", "signal", sig.String())
			cancel()
		case <-ctx.Done():
		}
	}()

	// Untrapping on the way out is what keeps a SECOND ^C working. A command
	// whose shutdown waits for something slow — `steps web` now waits for its
	// drain and poll loops — would otherwise swallow every further signal
	// into a channel nobody reads, and could only be killed with SIGKILL.
	// Once, since callers defer this and may also call it early themselves.
	return ctx, sync.OnceFunc(func() {
		signal.Stop(sigs)
		cancel()
	})
}

// applyHistoryFlags writes the command-line retention limits into the config
// unless the pipeline set its own.
//
// Precedence is resolved here rather than at the point of use so there is one
// place it lives: the pipeline wins, because it is the thing that knows what its
// resources and its jobs do. See config.VersionHistoryLimit and
// config.RunHistoryLimit.
func applyHistoryFlags(cfg *config.Config, versions, runs int) {
	if versions <= 0 && runs <= 0 {
		return
	}

	if cfg.Defaults == nil {
		cfg.Defaults = &config.Defaults{}
	}

	if versions > 0 && cfg.Defaults.VersionHistory == nil {
		cfg.Defaults.VersionHistory = &versions
	}

	if runs > 0 && cfg.Defaults.RunHistory == nil {
		cfg.Defaults.RunHistory = &runs
	}
}

// JobsCmd inspects and clears the watch circuit breaker.
//
// It exists because a paused job is otherwise invisible: the trigger loop stops
// triggering it and says so once, in output that has long since scrolled past
// by the time anyone wonders why the nightly summary stopped arriving.
type JobsCmd struct {
	List   JobsListCmd   `cmd:"" default:"withargs"                        help:"list jobs the circuit breaker has paused"`
	Resume JobsResumeCmd `cmd:"" help:"take a job out of the paused state"`
}

// JobsListCmd is the listing, and the group's default: bare `steps jobs
// <pipeline>` still answers "what has the breaker stopped?".
type JobsListCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
}

// Run prints every paused job.
func (j *JobsListCmd) Run() error {
	if nothingRecorded(j.Pipeline, j.StateFlags, "no jobs are paused") {
		return nil
	}

	st, cleanup, err := openStore(j.Pipeline, j.StateFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	paused, err := st.PausedJobs(context.Background())
	if err != nil {
		return fmt.Errorf("could not list paused jobs: %w", err)
	}

	if len(paused) == 0 {
		fmt.Println("no jobs are paused")

		return nil
	}

	writer := newTabWriter()

	_, _ = fmt.Fprintln(writer, "JOB\tCONSECUTIVE FAILURES\tPAUSED AT")

	for _, job := range paused {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\n", job.Name, job.Consecutive, job.PausedAt)
	}

	return flush(writer)
}

// JobsResumeCmd clears the breaker for one job.
//
// A subcommand rather than `jobs --resume <name>`: a flag that turns a
// listing into a write reads as configuration and behaves as a mutation, and
// the verb should say which one it is.
type JobsResumeCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
	Job        string `arg:""   help:"job to take out of the paused state"`
}

// Run resumes the named job.
func (j *JobsResumeCmd) Run() error {
	cfg, err := config.Load(j.Pipeline, resolvePipelineName(j.Pipeline, j.Name), nil)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	st, _, cleanup, err := setup(cfg, j.Pipeline, j.StateFlags, ExecFlags{})
	if err != nil {
		return err
	}
	defer cleanup()

	return resumeJob(context.Background(), cfg, st, j.Job)
}

// resumeJob takes a job back out of the paused state, refusing a name the
// pipeline does not have — a typo would otherwise report success while
// resuming nothing.
func resumeJob(ctx context.Context, cfg *config.Config, st *store.Store, name string) error {
	_, err := cfg.FindJob(name)
	if err != nil {
		return fmt.Errorf("cannot resume: %w", err)
	}

	err = st.ResetJobFailures(ctx, name)
	if err != nil {
		return fmt.Errorf("could not resume job %q: %w", name, err)
	}

	fmt.Printf("resumed: %s\n", name)

	return nil
}

// loadWithVars loads a pipeline with ((name)) substitution applied, from
// --var flags and an optional --vars-file.
//
// Flags win over the file: the file is the shared, checked-in set and the flag
// is the one-off override, which is the only ordering that makes overriding
// possible at all.
func loadWithVars(path string, name string, flags map[string]string, varsFile string) (*config.Config, error) {
	vars, err := resolveVars(flags, varsFile)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(path, name, vars)
	if err != nil {
		return nil, fmt.Errorf("could not load pipeline: %w", err)
	}

	return cfg, nil
}

// resolveVars gathers the ((name)) substitutions from --vars-file and --var.
//
// Flags win over the file: the file is the shared, checked-in set and the flag
// is the one-off override, which is the only ordering that makes overriding
// possible at all.
func resolveVars(flags map[string]string, varsFile string) (map[string]string, error) {
	vars := map[string]string{}

	if varsFile != "" {
		body, err := os.ReadFile(varsFile) //nolint:gosec // the vars file is one the operator named
		if err != nil {
			return nil, fmt.Errorf("could not read vars file %q: %w", varsFile, err)
		}

		var fromFile map[string]string

		err = yaml.Unmarshal(body, &fromFile)
		if err != nil {
			return nil, fmt.Errorf("could not parse vars file %q: %w", varsFile, err)
		}

		for name, value := range fromFile {
			vars[name] = value
		}
	}

	for name, value := range flags {
		vars[name] = value
	}

	return vars, nil
}

// WebCmd is the daemon: it serves the pipeline UI, polls every trigger: true
// resource, and runs the jobs both of those enqueue.
//
// One command rather than two, since `steps watch` was this minus the UI and
// running both against one state database was never a supported pairing —
// they claim each other's work, and startup recovery reads the other's
// in-flight rows as abandoned. --once is the cron form of the same cycle:
// poll, drain, exit, without binding anything.
//
// One or more pipeline files: state is per-pipeline by construction
// (.steps/state.db lives beside each YAML), so serving several means opening
// several stores, and the UI routes them under /p/<name>/.
//
// It polls trigger: true resources as well as serving, because a front end
// that drains a queue nothing fills is a runner that looks alive and notices
// nothing — the surprise this default exists to remove. There is no way to
// turn it off: a served process that does not poll is the half-daemon this
// command absorbed `steps watch` to stop being.
//
// It binds loopback by default and has no authentication, because there is
// nothing to authenticate against — this is the local runner's own front end,
// in the same trust domain as the shell that started it. Binding it to a
// routable address publishes trigger and approval controls to anyone who can
// reach the port; --listen exists for the person who has decided that is what
// they want, not as a default.
type WebCmd struct {
	StateFlags    `embed:""`
	VarFlags      `embed:""`
	ExecFlags     `embed:""`
	HistoryFlags  `embed:""`
	Pipeline      []string          `arg:""                                                                       help:"path(s) to pipeline YAML files"`
	Listen        string            `default:"127.0.0.1:8088"                                                     help:"address to serve on"`
	Interval      time.Duration     `default:"30s"                                                                help:"how often to check trigger: true resources"`
	Once          bool              `help:"poll once, run whatever that triggers, and exit (for cron or a timer)" name:"once"`
	MaxConcurrent int               `default:"1"                                                                  help:"maximum number of queued jobs running at once, per pipeline"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                      name:"pin"`
	Force         bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	ReadOnly      bool              `help:"serve without trigger, approval, or resume controls"                   name:"read-only"`
}

// Run loads every named pipeline, opens its store, and serves until canceled.
func (w *WebCmd) Run() error {
	// Rejected rather than shrugged at: a server that quietly served forever
	// without ever polling would be the exact confusion this command's
	// polling default exists to remove.
	if w.Interval <= 0 {
		return fmt.Errorf("web: --interval must be positive, got %s", w.Interval)
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx, err := w.ExecFlags.Apply(ctx)
	if err != nil {
		return err
	}

	pipelines, providers, cleanup, err := w.load()
	if err != nil {
		return err
	}
	defer cleanup()

	if w.Once {
		return w.runOnce(ctx, pipelines, providers)
	}

	return w.serve(ctx, pipelines, providers)
}

// runOnce polls each served pipeline once, runs whatever that enqueues, and
// returns — without binding the listen address.
//
// This is `steps web --once`, which is how steps is driven by something
// that already owns the schedule: cron, a systemd timer, a CI step. It does
// NOT serve, deliberately: a port opened for the duration of one poll is a
// port nothing has time to reach, and a one-shot that left a listener behind
// would be a daemon with extra steps.
//
// Serial across pipelines, like the watcher it replaces. A one-shot has
// nothing to stay responsive for.
func (w *WebCmd) runOnce(ctx context.Context, pipelines []*web.Pipeline, providers map[string]workspace.Provider) error {
	for _, target := range pipelines {
		w.HistoryFlags.Apply(target.Config())

		provider, ok := providers[target.Slug]
		if !ok {
			return fmt.Errorf("web: no workspace provider for pipeline %q", target.Slug)
		}

		err := trigger.WatchOnce(ctx, target.Config(), provider, target.Store, w.Pin, w.Force)

		// A pipeline with nothing to poll is not this command failing, and
		// the served path already says so per pipeline. Answering the same
		// pipeline two different ways depending on one flag is the shape
		// this consolidation exists to remove — and returning here would
		// also abandon every pipeline named after it, which is the whole
		// point of a `steps web --once app.yml infra.yml` cron line.
		if errors.Is(err, trigger.ErrNoTriggers) {
			fmt.Printf("steps web: %s has no trigger: true get; nothing to poll\n", target.Slug)

			continue
		}

		if err != nil {
			return wrapRunErr(err)
		}
	}

	return nil
}

// serve runs the UI, the poller and the drainer until the process is stopped.
//
// One process fills and drains the queue, because there is one daemon: a
// front end that drains a queue nothing fills is a runner that looks alive
// and notices nothing, and a second `steps web` on the same state database is
// the deployment mistake the one-process-per-file rule already names.
func (w *WebCmd) serve(ctx context.Context, pipelines []*web.Pipeline, providers map[string]workspace.Provider) error {
	// Before either loop below exists, because ResetStaleRunning is only safe
	// with no concurrent writer — see web.PrepareQueue. This process owns the
	// queue, which is what makes recovery correct: every `running` row is a
	// leftover of a process that is gone.
	//
	// Then the configuration is ADOPTED through the same function a reload
	// uses (ConfigWatcher.adopt), rather than through a startup copy of some
	// of what that function does. The copy is how `steps web --run-history 5`
	// came to work at startup and stop working a second later: two lists,
	// only one of them maintained.
	watchers := make([]*ConfigWatcher, 0, len(pipelines))

	for _, target := range pipelines {
		web.PrepareQueue(ctx, target)

		// The METHOD, not its result, for the reason trigger.Poll takes one:
		// the watcher swaps the configuration under this handler, and a
		// pipeline that names no webhook_token_env: resource today may name
		// one after the next save. /p/<slug>/check/<resource> is still a 404
		// while there are none — decided per request now, rather than once.
		// Mounted HERE, with the daemon's context, because a placed check
		// resolves its worker through what --worker put on it.
		target.Webhook = trigger.WebhookHandler(ctx, target.Config, target.Store)

		watcher := NewConfigWatcher(target, w.VarFlags, w.HistoryFlags)
		watchers = append(watchers, watcher)

		err := watcher.adopt(ctx, target.Config())
		if err != nil {
			return fmt.Errorf("web: %s: %w", target.Slug, err)
		}
	}

	var runner web.Runner

	local := web.NewLocalRunner(providers, w.Pin, w.MaxConcurrent, w.Force)
	if !w.ReadOnly {
		runner = local
	}

	server, err := web.New(pipelines, runner)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	ctx, cancel := context.WithCancel(ctx)

	var background sync.WaitGroup

	// Registered before the loops start, and it cancels rather than relying
	// on the caller's deferred cancel: Start also returns on its own error,
	// with the context still live and the loops still running — and both of
	// them write through stores the caller's cleanup closes.
	defer func() {
		cancel()
		background.Wait()
	}()

	// The drainer runs regardless of --read-only: a row queued before this
	// process started is still work it can do. What --read-only withholds is
	// the UI's ability to ADD work.
	background.Add(1)

	go func() {
		defer background.Done()

		local.Drain(ctx, pipelines)
	}()

	w.startPolling(ctx, &background, pipelines)
	w.startWatchingConfig(ctx, &background, watchers)

	fmt.Printf("steps web: http://%s\n", w.Listen)

	err = server.Start(ctx, w.Listen)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// startPolling launches one trigger poller per served pipeline, so this
// process both fills and drains the queue, which is what makes it the whole
// daemon rather than half of one.
//
// Each poller is handed its pipeline's OWN store handle rather than opening a
// second one; trigger.Poll's doc comment says why, and is the one copy of
// that reasoning.
//
// --read-only does not disable polling, deliberately: it withholds the
// BROWSER's ability to add work, which is a statement about the HTTP surface,
// not about what this process does on its own. `--listen 0.0.0.0 --read-only`
// is a build box that still has to notice new versions.
func (w *WebCmd) startPolling(ctx context.Context, background *sync.WaitGroup, pipelines []*web.Pipeline) {
	// One per served pipeline, unconditionally — including the ones with
	// nothing to poll right now.
	//
	// Deciding at startup which pipelines were worth a loop is what made a
	// `trigger: true` added by an edit go unchecked until a restart: the
	// decision had been taken once, against a file that has since changed.
	// The loop itself re-decides per configuration and says which way it went
	// (see trigger.Poll), which is also where the per-pipeline banner moved
	// to — it is a statement about what is being polled, and that is now a
	// thing that changes while the daemon runs.
	for _, target := range pipelines {
		fmt.Printf("steps web: watching %s, checking every %s\n", target.Slug, w.Interval)

		background.Add(1)

		go func() {
			defer background.Done()

			// The METHOD, not its result: the watcher swaps the
			// configuration under this loop, and a value taken here would
			// pin it to whatever the file said at startup.
			err := trigger.Poll(ctx, target.Config, target.Store, w.Interval)
			if err != nil {
				slog.Error("web.poll_stopped", "pipeline", target.Slug, "error", err)
			}
		}()
	}
}

// reloadInterval is how often the daemon re-reads its pipeline files.
//
// A constant rather than a flag: it is one read of a small local file, so
// there is nothing to tune — the number exists only because a save should
// take effect at human speed, and every value between "immediately" and "a
// second later" reads the same to whoever pressed save.
const reloadInterval = time.Second

// startWatchingConfig keeps every served pipeline in step with the file it
// was loaded from, so an edit no longer needs a restart to take effect.
//
// Not gated on --read-only, for the same reason polling is not: that flag
// withholds the BROWSER's ability to start work. The file on disk is the
// operator's own statement of what this daemon serves, and a build box that
// ignored it would be one more thing to remember to restart.
func (w *WebCmd) startWatchingConfig(ctx context.Context, background *sync.WaitGroup, watchers []*ConfigWatcher) {
	for _, watcher := range watchers {
		background.Add(1)

		go func() {
			defer background.Done()

			watcher.Watch(ctx, reloadInterval)
		}()
	}
}

// load opens every pipeline named on the command line, along with its store,
// workspace provider, and event bus.
func (w *WebCmd) load() ([]*web.Pipeline, map[string]workspace.Provider, func(), error) {
	err := w.checkNamesAreDistinct()
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		pipelines []*web.Pipeline
		closers   []func()
	)

	providers := map[string]workspace.Provider{}

	cleanup := func() {
		for _, close := range closers {
			close()
		}
	}

	for _, path := range w.Pipeline {
		// Resolved before the load, not after it: the slug IS the Config's
		// identity, and computing it four lines later is how the two came to
		// disagree.
		slug := resolvePipelineName(path, w.Name)

		cfg, err := w.Load(path, slug)
		if err != nil {
			cleanup()

			return nil, nil, nil, err
		}

		st, provider, closeOne, err := setup(cfg, path, w.StateFlags, w.ExecFlags)
		if err != nil {
			cleanup()

			return nil, nil, nil, err
		}

		bus := events.New(pipeline.StoreSink(st))

		// Bus first, store second: cleanup runs these in order, and the bus
		// drains its queued events INTO the store. Closing the store first
		// would throw away the tail of whatever run was in flight.
		closers = append(closers, bus.Close, closeOne)

		providers[slug] = provider
		served := web.NewPipeline(slug, path, cfg, st, bus)
		pipelines = append(pipelines, served)
	}

	return pipelines, providers, cleanup, nil
}

// checkNamesAreDistinct refuses two pipelines that would answer to one name.
//
// Before any store is opened, deliberately: a name IS a pipeline's identity in
// the state database, so opening them first would register both against one
// row and leave the second's path overwriting the first's before the error
// surfaced. Two app/pipeline.yml and infra/pipeline.yml under one --state is
// the case, and it is a question only the operator can answer — hence --name
// rather than a generated suffix, which would be an identity nobody could
// predict and every rerun would have to rediscover.
func (w *WebCmd) checkNamesAreDistinct() error {
	seen := map[string]string{}

	for _, path := range w.Pipeline {
		name := resolvePipelineName(path, w.Name)

		if other, clash := seen[name]; clash {
			return fmt.Errorf(
				"web: %s and %s are both named %q; give one a distinct --name, e.g. --name %s-2=%s",
				other, path, name, name, path)
		}

		seen[name] = path
	}

	return nil
}

// ApprovalsCmd lists approval: steps waiting for a decision.
//
// A parked approval that nobody is told about is useless in practice, so this
// is the "what is waiting on me?" command. It reads the same rows the audit
// trail is made of.
type ApprovalsCmd struct {
	List    ApprovalsListCmd `cmd:"" default:"withargs"                      help:"list approval: steps waiting for a decision"`
	Approve ApproveCmd       `cmd:"" help:"approve a waiting approval: step"`
	Reject  RejectCmd        `cmd:"" help:"reject a waiting approval: step"`
}

// ApprovalsListCmd is the listing itself, and the group's default: bare
// `steps approvals <pipeline>` still answers "what is waiting on me?".
type ApprovalsListCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
}

// Run prints every pending approval.
func (a *ApprovalsListCmd) Run() error {
	if nothingRecorded(a.Pipeline, a.StateFlags, "no approvals are waiting") {
		return nil
	}

	st, cleanup, err := openStore(a.Pipeline, a.StateFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	pending, err := st.PendingApprovals(context.Background())
	if err != nil {
		return fmt.Errorf("could not list approvals: %w", err)
	}

	if len(pending) == 0 {
		fmt.Println("no approvals are waiting")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "ID\tJOB\tREQUESTED\tMESSAGE")

	for _, approval := range pending {
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\n",
			approval.ID, approval.JobName, approval.RequestedAt, approval.Message)
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the approvals table: %w", err)
	}

	return nil
}

// ApproveCmd records a yes.
//
// ⚠️ v1 scope, stated deliberately rather than discovered: anyone who can run
// this command can approve. There is no separate identity system — the
// recorded approver is the OS user, which is an audit record, not an
// authorization check. Someone will ask "can anyone approve?" the day this
// ships, and the answer is yes, on purpose, for now.
type ApproveCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""                                       help:"path to the pipeline YAML file"`
	ID         int64  `arg:""                                       help:"the approval id, from steps approvals"`
	Reason     string `help:"note to record alongside the decision"`
}

// Run approves the named approval.
func (a *ApproveCmd) Run() error {
	return decideApproval(a.Pipeline, a.StateFlags, a.ID, "approved", a.Reason)
}

// RejectCmd records a no.
type RejectCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""                                    help:"path to the pipeline YAML file"`
	ID         int64  `arg:""                                    help:"the approval id, from steps approvals"`
	Reason     string `help:"why — recorded with the decision"`
}

// Run rejects the named approval.
func (r *RejectCmd) Run() error {
	return decideApproval(r.Pipeline, r.StateFlags, r.ID, "rejected", r.Reason)
}

// decideApproval records a decision against a pipeline's store.
func decideApproval(pipelinePath string, flags StateFlags, id int64, status, reason string) error {
	st, cleanup, err := openStore(pipelinePath, flags)
	if err != nil {
		return err
	}
	defer cleanup()

	err = st.DecideApproval(context.Background(), id, status, currentUser(), reason)
	if err != nil {
		return fmt.Errorf("could not record the decision: %w", err)
	}

	fmt.Printf("%s: approval %d\n", status, id)

	return nil
}

// QuestionsCmd is the "what is waiting on me?" command for questions, the way
// ApprovalsCmd is for approvals. It reads the same rows the audit trail is
// made of.
//
// Separate from `steps approvals` because the two park for different reasons
// and are answered differently: an approval takes a yes or a no, a question
// takes a fact — and a listing that mixed them would have to leave out the
// options, which are most of what makes a question one keystroke to answer.
type QuestionsCmd struct {
	List   QuestionsListCmd `cmd:"" default:"withargs"                        help:"list ask_user questions waiting for an answer"`
	Answer AnswerCmd        `cmd:"" help:"answer a waiting ask_user question"`
}

// QuestionsListCmd is the listing itself, and the group's default.
type QuestionsListCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
}

// Run prints every question waiting for an answer.
func (q *QuestionsListCmd) Run() error {
	if nothingRecorded(q.Pipeline, q.StateFlags, "no questions are waiting") {
		return nil
	}

	st, cleanup, err := openStore(q.Pipeline, q.StateFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	pending, err := st.PendingQuestions(context.Background())
	if err != nil {
		return fmt.Errorf("could not list questions: %w", err)
	}

	if len(pending) == 0 {
		fmt.Println("no questions are waiting")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "ID\tJOB\tSTEP\tASKED\tQUESTION")

	for _, question := range pending {
		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\n",
			question.ID, question.JobName, question.AgentName, question.AskedAt, question.Question)

		// Under the question rather than in a column of its own: an option
		// list is as long as the answers are, and squeezing it into a cell
		// makes the table unreadable exactly when it matters.
		if len(question.Options) > 0 {
			_, _ = fmt.Fprintf(writer, "\t\t\t\toptions: %s\n", strings.Join(question.Options, " | "))
		}
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the questions table: %w", err)
	}

	return nil
}

// AnswerCmd supplies the fact a parked agent step is waiting on.
//
// ⚠️ Same v1 scope as ApproveCmd, stated deliberately: anyone who can run this
// command can answer. The recorded answerer is the OS user, which is an audit
// record and not an authorization check.
type AnswerCmd struct {
	StateFlags `embed:""`
	Pipeline   string `arg:""   help:"path to the pipeline YAML file"`
	ID         int64  `arg:""   help:"the question id, from steps questions"`
	// Variadic so an answer can be written without quoting it, which is what
	// somebody typing a sentence back at a parked step will do.
	Answer []string `arg:"" help:"the answer — one of the offered options, or your own words"`
}

// Run answers the named question.
func (a *AnswerCmd) Run() error {
	st, cleanup, err := openStore(a.Pipeline, a.StateFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	answer := strings.TrimSpace(strings.Join(a.Answer, " "))
	if answer == "" {
		return errors.New("an answer is required: steps questions answer <pipeline> <id> <answer>")
	}

	err = st.AnswerQuestion(context.Background(), a.ID, answer, currentUser())
	if err != nil {
		return fmt.Errorf("could not record the answer: %w", err)
	}

	fmt.Printf("answered: question %d\n", a.ID)

	return nil
}

// currentUser is the audit record's "who". It is deliberately not an
// authorization check: it records who ran the command on this host, which is
// what someone reconstructing a decision later needs.
func currentUser() string {
	for _, key := range []string{"STEPS_APPROVER", "USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}

	return "unknown"
}

// openStore opens a pipeline's already-recorded state for the approval,
// question and paused-job commands.
//
// The same resolve-never-register path `steps runs` takes, and for the same
// reason one level along: every caller here acts on a row that must already
// exist — an approval to decide, a question to answer, a breaker to read — so
// none of them has any business CREATING the pipeline it was named. It used
// to go through setup, which opened the store the writer's way and minted a
// pipelines row for a typo; worse, it did so in a state file `steps runs` had
// just learned to refuse that name in, so one listing quietly made the
// refusal stop working.
//
// It also builds no workspace provider, which setup did — a listing that
// creates and removes a temp root to print three rows.
func openStore(pipelinePath string, flags StateFlags) (*store.Store, func(), error) {
	return openRecorded(pipelinePath, flags)
}

// applyResume points this invocation at a previous run: which steps it need
// not repeat, and which workspace to continue in.
//
// The job name comes from the recorded run rather than the flag, so
// `--resume <id>` alone is enough — asking an operator to remember which job a
// run id belonged to would make the id useless on its own.
func applyResume(
	ctx context.Context, st *store.Store, provider workspace.Provider, runID, jobName string,
) (context.Context, string, error) {
	resumable, ok := provider.(workspace.Resumable)
	if !ok {
		// Every provider is resumable today; this stays as the honest answer
		// for one that is not, rather than resuming into a tree that cannot
		// hold the previous run's artifacts and calling it a recovery.
		return ctx, "", errors.New("--resume is not supported by this workspace provider")
	}

	ctx, dir, err := pipeline.PrepareResume(ctx, st, runID)
	if err != nil {
		return ctx, "", fmt.Errorf("could not resume: %w", err)
	}

	resumable.Reuse(dir)

	if jobName == "" {
		jobName, err = pipeline.ResumeJobName(ctx, st, runID)
		if err != nil {
			return ctx, "", fmt.Errorf("could not resume: %w", err)
		}
	}

	return ctx, jobName, nil
}

// printCostTotals lists what each recorded run's agent steps spent.
//
// The cache column is the one worth having: it is the only place prompt
// caching reports whether it did anything, and a run that suddenly drops from
// 60% to 0% is the visible half of a bill that doubled.
func (r *RunsCostCmd) printCostTotals(ctx context.Context, st *store.Store) error {
	totals, err := st.RunCostTotals(ctx, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read usage: %w", err)
	}

	if len(totals) == 0 {
		fmt.Println("no agent usage recorded yet")

		return nil
	}

	fmt.Printf("%-12s  %12s  %7s  %10s  %6s\n", "RUN", "TOKENS", "CACHED", "COST", "STEPS")

	for _, total := range totals {
		fmt.Printf("%-12s  %12s  %6s%%  %10s  %6d\n",
			total.RunID, humanTokens(total.Tokens), cachePercent(total.Tokens, total.Cached),
			renderCost(total.CostUSD, total.Unpriced), total.Steps)
	}

	fmt.Printf("\nbreak one down with: steps runs cost %s <run>%s\n", r.Pipeline, stateNote(r.State))

	return nil
}

// stateNote carries --state into a printed follow-up command.
//
// Without it the hint names a DIFFERENT database than the one it was just
// printed from: the default path is derived from the pipeline, so a reader who
// copies the line after `steps runs cost app.yml --state shared.db` is sent to
// `.steps/app.yml.db` and told there is nothing there.
func stateNote(state string) string {
	if state == "" {
		return ""
	}

	return " --state " + state
}

// printRunCost breaks one run down per agent step.
func (r *RunsCostCmd) printRunCost(ctx context.Context, st *store.Store) error {
	usage, err := st.RunUsage(ctx, r.RunID)
	if err != nil {
		return fmt.Errorf("could not read usage: %w", err)
	}

	if len(usage) == 0 {
		fmt.Printf("no agent usage recorded for run %s\n", r.RunID)

		return nil
	}

	fmt.Printf("%-28s  %12s  %7s  %9s  %s\n", "STEP", "TOKENS", "CACHED", "DURATION", "FINISH")

	for _, step := range usage {
		fmt.Printf("%-28s  %12s  %6s%%  %9s  %s\n",
			truncateName(step.StepName, 28), humanTokens(step.Total),
			cachePercent(step.Total, step.Cached),
			(time.Duration(step.DurationMS) * time.Millisecond).Round(time.Second),
			finishNote(step))
	}

	return nil
}

// printPlacements says which machines a run's placed steps ran on, and what
// those machines turned out to be.
//
// The answer to "it passes locally and fails on the fleet". Facts and never a
// price: what an instance-hour cost is not knowable from here — list prices
// ignore Savings Plans, a spot instance's paid price is reported by no API,
// and real billing lands a day later — and a confident wrong number in a cost
// column is worse than no column. Anyone holding their own rate card can
// price these rows.
//
// Rendered through web.PlacementView, the same type the run page draws: one
// spelling of what a machine was, so the browser and the terminal cannot
// disagree about it. They did — the terminal's copy never learned which
// filesystems are memory.
func (r *RunsWhereCmd) printPlacements(ctx context.Context, st *store.Store) error {
	run, ok, err := r.placementRun(ctx, st)
	if err != nil || !ok {
		return err
	}

	placements, err := st.RunPlacements(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("could not read placements: %w", err)
	}

	if len(placements) == 0 {
		// Distinguished from "this run had no placed steps", which is the
		// ordinary case for a pipeline that names no worker. Past tense only
		// about a run that is over: a placement is recorded when the step
		// finishes, so a run still in flight has nothing recorded YET.
		if run.Status == "running" {
			fmt.Printf("no placed steps recorded for run %s yet\n", run.ID)
		} else {
			fmt.Printf("run %s ran every step on this machine\n", run.ID)
		}

		return nil
	}

	fmt.Printf("%-24s  %-12s  %-13s  %-28s  %9s  %s\n",
		"STEP", "TAG", "PLATFORM", "FILESYSTEM", "SENT", "MACHINE")

	memory := false

	for _, placed := range placements {
		view := web.PlacementView{Placement: placed}

		filesystem := view.Filesystem()
		if view.Volatile() {
			filesystem += " [RAM]"
			memory = true
		}

		fmt.Printf("%-24s  %-12s  %-13s  %-28s  %9s  %s\n",
			truncateName(placed.StepName, 24), truncateName(placed.Tag, 12),
			view.Platform(), filesystem, view.Sent(), view.Machine())
	}

	if memory {
		fmt.Println("\n[RAM] that workdir is memory, not disk: the pushed binary and the step's tree spend it, and a reboot loses both. Name a path on a real disk in the worker URL.")
	}

	return nil
}

// placementRun is the run --where reports on: the one named, or the newest.
//
// A named run is looked up rather than taken on trust. RunPlacements is
// pipeline-scoped, so a typo — or a run belonging to another pipeline sharing
// this state file — reads back as zero rows, and the caller would print that
// as a run that ran every step here: a positive claim about a run this
// pipeline has never seen.
func (r *RunsWhereCmd) placementRun(ctx context.Context, st *store.Store) (store.RunRow, bool, error) {
	if r.RunID != "" {
		run, ok, err := st.FindRunRow(ctx, r.RunID)
		if err != nil {
			return store.RunRow{}, false, fmt.Errorf("could not read run: %w", err)
		}

		if !ok {
			fmt.Printf("no run %s in this pipeline\n", r.RunID)
		}

		return run, ok, nil
	}

	runs, err := st.ListRuns(ctx, r.Job, 1)
	if err != nil {
		return store.RunRow{}, false, fmt.Errorf("could not read runs: %w", err)
	}

	if len(runs) == 0 {
		fmt.Println("no runs recorded")

		return store.RunRow{}, false, nil
	}

	return runs[0], true, nil
}

// humanTokens groups a token count with thin separators, so 4102338 reads as
// a number rather than a smear of digits.
func humanTokens(n int) string {
	digits := strconv.Itoa(n)
	if len(digits) <= 3 {
		return digits
	}

	var out strings.Builder

	lead := len(digits) % 3
	if lead > 0 {
		out.WriteString(digits[:lead])
	}

	for i := lead; i < len(digits); i += 3 {
		if out.Len() > 0 {
			out.WriteString(",")
		}

		out.WriteString(digits[i : i+3])
	}

	return out.String()
}

// cachePercent renders the share of a step's tokens the provider served from
// cache, or "-" when it reported no usage at all (0 of 0 is not 0%).
func cachePercent(total, cached int) string {
	if total <= 0 {
		return "-"
	}

	return strconv.Itoa(cached * 100 / total)
}

// renderCost shows a dollar figure only when something actually reported one,
// and marks it partial when some steps did not.
//
// Never $0.00 for an unreported cost: only a CLI-backed agent reports dollars
// at all, so a zero here would say every hosted run was free rather than that
// nobody priced it. See the agent_usage schema comment.
//
// web.FormatUSD rather than a local %.2f, and for the same reason the absent
// case is not a zero: a CLI step routinely costs fractions of a cent, and two
// decimals round exactly those runs to the "$0.00" this function exists to
// never print.
func renderCost(cost *float64, unpriced int) string {
	if cost == nil {
		return "unpriced"
	}

	rendered := web.FormatUSD(*cost)
	if unpriced > 0 {
		rendered += fmt.Sprintf("+%d?", unpriced)
	}

	return rendered
}

// finishNote says how a step's last response ended, calling out the one value
// that is a defect rather than an outcome.
//
// A response cut off by max_tokens reads exactly like a model that had little
// to say — and a truncated verdict or JSON body is the failure mode that
// wastes a whole downstream step. Naming it here is the cheapest place to
// notice.
func finishNote(step store.AgentUsage) string {
	switch {
	case step.FinishReason == "":
		return "-"
	case strings.EqualFold(step.FinishReason, "length"), strings.EqualFold(step.FinishReason, "max_tokens"):
		return step.FinishReason + "  <-- truncated"
	default:
		return step.FinishReason
	}
}

func truncateName(name string, width int) string {
	if len(name) <= width {
		return name
	}

	return name[:width-1] + "…"
}
