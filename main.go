// Package main implements steps, a small CLI that interprets a
// Concourse-style pipeline YAML file (resource_types/resources/jobs):
// check discovers resource versions, get fetches one via a rendered
// shell command, and task runs a plan step's command. `run` executes one
// job once; `watch` polls trigger: true resources and auto-runs every job
// a changed resource affects.
package main

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

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/trigger"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/workspace"
)

// CLI is the pipeline runner's command-line grammar, parsed by kong. Run is
// default:"withargs" so today's flat invocation (steps pipeline.yml --job x)
// keeps working unchanged, routed to it implicitly. LogLevel is a global flag
// (available to every subcommand, not just Run) rather than living on RunCmd/
// WatchCmd/TestCmd individually, since it configures the process-wide slog
// default logger before any subcommand's Run method executes — see
// initLogging.
type CLI struct {
	LogLevel  string           `default:"info"                          enum:"debug,info,warn,error"                                          env:"STEPS_LOG_LEVEL"        help:"log verbosity: debug, info, warn, or error"`
	Version   kong.VersionFlag `help:"print the steps version and exit" name:"version"`
	Run       RunCmd           `cmd:""                                  default:"withargs"                                                    help:"run a single job once"`
	Watch     WatchCmd         `cmd:""                                  help:"poll trigger: true resources and auto-run affected jobs"`
	Test      TestCmd          `cmd:""                                  help:"run every job (force) and verify assert directives"`
	Validate  ValidateCmd      `cmd:""                                  help:"check a pipeline for errors without running anything"`
	Runs      RunsCmd          `cmd:""                                  help:"show what past runs recorded"`
	Plan      PlanCmd          `cmd:""                                  help:"show which steps a run would execute or skip"`
	MCP       MCPCmd           `cmd:""                                  help:"inspect or authorize a pipeline's mcp_servers: entries"`
	Preflight PreflightCmd     `cmd:""                                  help:"check a job's models and MCP servers are live, running nothing"`
	Jobs      JobsCmd          `cmd:""                                  help:"list jobs the watch circuit breaker has paused, or resume one"`
	Approvals ApprovalsCmd     `cmd:""                                  help:"list approval: steps waiting for a decision"`
	Approve   ApproveCmd       `cmd:""                                  help:"approve a waiting approval: step"`
	Reject    RejectCmd        `cmd:""                                  help:"reject a waiting approval: step"`
	Web       WebCmd           `cmd:""                                  help:"serve the pipeline UI over the same state the CLI writes"`
	Docs      DocsCmd          `cmd:""                                  help:"read the docs in the terminal (no page name lists them)"`
}

// buildVersion is the version string steps --version prints. Overridden at
// build time via -ldflags "-X main.buildVersion=...";  "dev" covers `go run`/
// unversioned `go build` invocations.
var buildVersion = "dev"

// RunCmd runs a single job's plan once, exactly as steps has always done.
type RunCmd struct {
	Pipeline       string            `arg:""                                                                                         help:"path to the pipeline YAML file"`
	Job            string            `help:"job name to run (defaults to the pipeline's only job)"`
	Pin            map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                                        name:"pin"`
	Force          bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	KeepWorkspace  bool              `env:"STEPS_KEEP_WORKSPACE"                                                                     help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight    bool              `help:"skip the pre-run health check of the job's models and MCP servers"                       name:"no-preflight"`
	Var            map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..."                                     name:"var"`
	Resume         string            `help:"continue a failed run from the step that failed"                                         name:"resume"`
	Replay         string            `help:"fork a recorded run and re-run it from --from onward"                                    name:"replay"`
	From           string            `help:"with --replay, the step name to re-run from"                                             name:"from"`
	VarsFile       string            `help:"YAML file of pipeline vars"                                                              name:"vars-file"`
	VersionHistory int               `help:"how many versions of each resource to remember (pipeline defaults.version_history wins)" name:"version-history"`
	RunHistory     int               `help:"how many runs of each job to keep (pipeline defaults.run_history wins)"                  name:"run-history"`
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
	ctx context.Context, st *store.Store, provider workspace.Provider, job *config.Job,
) (context.Context, error) {
	if r.Replay == "" {
		return ctx, nil
	}

	if r.From == "" {
		return ctx, errors.New("--replay needs --from <step>: a replay re-runs from a step you name, and without one it would just re-run the whole plan")
	}

	ctx, _, err := pipeline.PrepareReplay(ctx, st, provider, r.Replay, r.From, job)
	if err != nil {
		return ctx, fmt.Errorf("could not replay: %w", err)
	}

	return ctx, nil
}

// Run loads the pipeline, selects a job, and runs it once via
// pipeline.RunJob.
func (r *RunCmd) Run() error {
	cfg, err := loadWithVars(r.Pipeline, r.Var, r.VarsFile)
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, r.Pipeline, r.KeepWorkspace)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	// --version-history was declared on this command but never applied, so it
	// silently did nothing here and only worked under `steps watch`. Both limits
	// belong on both commands: a manual run records the same nodes, events and
	// transcripts a triggered one does.
	applyHistoryFlags(cfg, r.VersionHistory, r.RunHistory)

	ctx = applyPreflightFlag(ctx, r.NoPreflight)

	jobName := r.Job

	ctx, jobName, err = r.applyContinuation(ctx, st, provider, jobName)
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, jobName)
	if err != nil {
		return err
	}

	ctx, err = r.applyReplay(ctx, st, provider, job)
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

// WatchCmd polls every resource named by a trigger:true get step, across
// every job in the pipeline, and runs whichever jobs a version change
// affects — see internal/trigger.
type WatchCmd struct {
	Pipeline       string            `arg:""                                                                                         help:"path to the pipeline YAML file"`
	Interval       time.Duration     `default:"30s"                                                                                  help:"how often to check trigger: true resources"`
	Once           bool              `help:"poll once, run whatever that triggers, and exit (for cron or a timer)"                   name:"once"`
	VersionHistory int               `help:"how many versions of each resource to remember (pipeline defaults.version_history wins)" name:"version-history"`
	RunHistory     int               `help:"how many runs of each job to keep (pipeline defaults.run_history wins)"                  name:"run-history"`
	MaxConcurrent  int               `default:"1"                                                                                    help:"maximum number of triggered jobs running at once"`
	Pin            map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                                        name:"pin"`
	Force          bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	KeepWorkspace  bool              `env:"STEPS_KEEP_WORKSPACE"                                                                     help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight    bool              `help:"skip the pre-run health check of each job's models and MCP servers"                      name:"no-preflight"`
	Listen         string            `help:"serve webhook checks on this address, e.g. :8080"                                        name:"listen"`
	Var            map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..."                                     name:"var"`
	VarsFile       string            `help:"YAML file of pipeline vars"                                                              name:"vars-file"`
}

// Run loads the pipeline and blocks in trigger.Watch until canceled
// (SIGINT/SIGTERM), or polls exactly once under --once.
func (w *WatchCmd) Run() error {
	cfg, err := loadWithVars(w.Pipeline, w.Var, w.VarsFile)
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, w.Pipeline, w.KeepWorkspace)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	applyHistoryFlags(cfg, w.VersionHistory, w.RunHistory)

	ctx = applyPreflightFlag(ctx, w.NoPreflight)

	slog.Info("pipeline.watch", "pipeline", w.Pipeline, "once", w.Once, "interval", w.Interval, "max_concurrent", w.MaxConcurrent)

	if w.Once {
		return wrapRunErr(trigger.WatchOnce(ctx, cfg, provider, st, w.Pin, w.Force))
	}

	return wrapRunErr(trigger.Watch(ctx, cfg, provider, st, w.Pin, w.Interval, w.MaxConcurrent, w.Force, w.Listen))
}

// TestCmd runs every job in the pipeline (force, so nothing is skipped and the
// recorded execution order is deterministic) and verifies its assert:
// directives — each job's own assert.execution is checked inside RunJob, and a
// top-level assert.execution of job names is checked here. It's the entry
// point for a self-verifying fixture — every runnable example in docs/*.md
// is one (see docs_test.go).
type TestCmd struct {
	Pipeline string            `arg:""                                                     help:"path to the pipeline YAML file"`
	Var      map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..." name:"var"`
	VarsFile string            `help:"YAML file of pipeline vars"                          name:"vars-file"`
}

// Run loads the pipeline, runs every job (force), and reports pass/fail per
// job plus the pipeline-level assert.execution. It returns a non-nil error if
// any job failed or the pipeline assert mismatched, so the process exits
// non-zero.
func (t *TestCmd) Run() error {
	cfg, err := loadWithVars(t.Pipeline, t.Var, t.VarsFile)
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, t.Pipeline, false)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

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
	Pipeline string `arg:"" help:"path to the pipeline YAML file"`
	// SyntaxOnly skips the checks about THIS MACHINE — credentials and MCP
	// binaries — leaving only the checks about the file. It exists for the
	// lint-in-CI case: a pre-commit hook or a build that checks a pipeline it
	// has no intention of running should not need that pipeline's production
	// credentials on hand.
	SyntaxOnly bool              `help:"check the file only; skip credential and MCP-binary checks about this machine" name:"syntax-only"`
	Var        map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..."                           name:"var"`
	VarsFile   string            `help:"YAML file of pipeline vars"                                                    name:"vars-file"`
}

// Run loads the pipeline (which runs every config-level validator) and then
// checks artifact flow for each job, joining the failures so one invocation
// reports everything wrong with the file.
func (v *ValidateCmd) Run() error {
	cfg, err := loadWithVars(v.Pipeline, v.Var, v.VarsFile)
	if err != nil {
		return err
	}

	var errs []error

	// An unparsable expr: expression is a fact about the FILE, so it is
	// checked here rather than only at preflight — and before --syntax-only
	// can skip anything, since nothing about it depends on this machine.
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

	err = errors.Join(errs...)
	if err != nil {
		return err
	}

	// "ok" has to mean "this will run", not "the YAML parses". The checks
	// above are all about the file; these are about the machine — credentials
	// the agents need and binaries the MCP servers need, both knowable in
	// microseconds and both, before this, discovered only at run time, after
	// an agent step had already started billing.
	//
	// They live here rather than in LoadConfig deliberately: an absent key is
	// a fact about this machine right now, so making it a load error would
	// break `steps plan` on a laptop and any CI job that lints a pipeline it
	// does not run.
	problems := []config.Problem{}
	if !v.SyntaxOnly {
		problems = cfg.CheckEnvironment()
	}

	if len(problems) > 0 {
		return fmt.Errorf("%s cannot run here:\n%s", v.Pipeline, renderProblems(problems))
	}

	fmt.Printf("ok: %s (%d job(s), %d resource(s), %d agent(s))\n",
		v.Pipeline, len(cfg.Jobs), len(cfg.Resources), len(cfg.Agents))

	return nil
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
	Pipeline string            `arg:""                                                     help:"path to the pipeline YAML file"`
	Job      string            `help:"job to plan (defaults to the pipeline's only job)"`
	Pin      map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"    name:"pin"`
	Var      map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..." name:"var"`
	VarsFile string            `help:"YAML file of pipeline vars"                          name:"vars-file"`
}

// Run loads the pipeline, plans the selected job, and prints one line per
// step. Resource check commands run (planning has always resolved get
// versions), but no step executes and nothing is recorded.
func (p *PlanCmd) Run() error {
	cfg, err := loadWithVars(p.Pipeline, p.Var, p.VarsFile)
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, p.Job)
	if err != nil {
		return err
	}

	st, err := store.OpenStore(statePath(p.Pipeline))
	if err != nil {
		return fmt.Errorf("could not open state store: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

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

// RunsCmd prints what past runs recorded — job outcomes by default, the
// per-step detail with --steps, and what `steps watch` has queued with
// --queue.
//
// The store has always written all of this and offered no way to read it: the
// only route to "why did my last run fail" was opening .steps/state.db in
// sqlite and knowing the schema, which the vendored pure-Go driver means may
// not even be installed.
type RunsCmd struct {
	Pipeline string `arg:""                                                  help:"path to the pipeline YAML file"`
	Job      string `help:"only show runs of this job"`
	Limit    int    `default:"20"                                            help:"maximum number of rows to show"`
	Steps    bool   `help:"show individual steps instead of job outcomes"`
	Queue    bool   `help:"show the watch trigger queue instead of job runs"`
	Cost     bool   `help:"show what each run's agent steps spent"`
	RunID    string `help:"break one run's agent spend down per step"        name:"run"`
}

// Run opens the pipeline's state store read-only and prints the requested
// view. It stats the database first so asking about history never creates
// one: a read command that leaves a .steps/ behind would be a surprising
// thing for `steps runs` to do on a fresh checkout.
func (r *RunsCmd) Run() error {
	path := statePath(r.Pipeline)

	_, err := os.Stat(path)
	if err != nil {
		fmt.Printf("no runs recorded yet for %s\n", r.Pipeline)

		return nil
	}

	st, err := store.OpenStore(path)
	if err != nil {
		return fmt.Errorf("could not open state store: %w", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	switch {
	// --run implies the per-step breakdown rather than needing --cost beside
	// it. Naming a run is unambiguous about what is wanted, and a flag that
	// reads as configured while binding nothing is the shape this codebase
	// rejects everywhere else.
	case r.RunID != "":
		return r.printRunCost(ctx, st)
	case r.Cost:
		return r.printCostTotals(ctx, st)
	case r.Queue:
		return r.printQueue(ctx, st)
	case r.Steps:
		return r.printSteps(ctx, st)
	default:
		return r.printJobRuns(ctx, st)
	}
}

func (r *RunsCmd) printJobRuns(ctx context.Context, st *store.Store) error {
	rows, err := st.ListJobRuns(ctx, r.Job, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read job runs: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no job runs recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tJOB\tSTATUS\tERROR")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n",
			formatWhen(row.CreatedAt), row.JobName, row.Status, firstLine(row.Error))
	}

	return flush(writer)
}

func (r *RunsCmd) printSteps(ctx context.Context, st *store.Store) error {
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

func (r *RunsCmd) printQueue(ctx context.Context, st *store.Store) error {
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
// an ✗, not a non-zero exit. `steps preflight` is the verb that fails.
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

// defaultLogLevel is what initLogging runs at before the CLI's own
// --log-level/STEPS_LOG_LEVEL has been parsed (during kong construction
// itself, and as CLI.LogLevel's own default) — never debug, so a parse
// failure (or any other pre-parse code path) can't fall back to printing
// every subsequent command/output dump by accident.
const defaultLogLevel = "info"

// parseLogLevel maps a --log-level/STEPS_LOG_LEVEL string to a slog.Level,
// falling back to slog.LevelInfo for anything unrecognized — reachable only
// if called before kong's own enum: validation on CLI.LogLevel runs (e.g.
// defaultLogLevel's own value, or a hypothetical future caller), never from
// a successfully parsed CLI. A standalone function (rather than inlined
// into initLogging) so it can be unit-tested without touching the global
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

// initLogging installs a slog handler on stderr as the default logger,
// separate from this tool's plain stdout progress lines ("get: prs
// (version: ...)", "task: review"), at the given level (see parseLogLevel).
// Debug is what previously ran unconditionally on every invocation: shell.go/
// docker.go log the full rendered command and complete captured stdout/
// stderr at that level, so any resource check/in/out command whose
// templated source: or output embeds a credential was written to stderr on
// every ordinary run, with no way to suppress it. Defaulting to info (see
// CLI.LogLevel) makes that opt-in, via --log-level debug or
// STEPS_LOG_LEVEL=debug, rather than the permanent default.
func initLogging(level string) {
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
// its own stderr is a live terminal. Checked at every initLogging call
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

func main() {
	initLogging(defaultLogLevel)

	err := run(os.Args[1:])
	if err != nil {
		code := outcome.ExitCode(err)

		slog.Debug("main.run", "error", err, "code", code)
		fmt.Fprintf(os.Stderr, "steps: error: %v\n", err)
		os.Exit(code)
	}

	slog.Debug("main.exit", "code", outcome.ExitOK)
}

func run(args []string) error {
	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Description("run pipeline jobs, or watch for trigger: true resource changes"), kong.Vars{"version": buildVersion})
	if err != nil {
		return fmt.Errorf("could not build CLI parser: %w", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return fmt.Errorf("could not parse flags: %w", err)
	}

	initLogging(cli.LogLevel)

	slog.Debug("cli.parse", "args", args)

	return kctx.Run() //nolint:wrapcheck // the Run methods above already wrap their own errors via wrapRunErr
}

// setup opens the state store and builds/validates the workspace provider
// shared by RunCmd and WatchCmd, returning a cleanup func that closes both
// (logging, not returning, any close error — mirroring the deferred
// close-error handling both commands used inline before this helper
// existed).
func setup(cfg *config.Config, pipelinePath string, keepWorkspace bool) (*store.Store, workspace.Provider, func(), error) {
	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not open state store: %w", err)
	}

	provider, err := workspace.NewProvider(cfg.Workspace, keepWorkspace)
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("could not build workspace provider: %w", err)
	}

	err = provider.Validate()
	if err != nil {
		_ = st.Close()

		return nil, nil, nil, fmt.Errorf("workspace: %w", err)
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

// wrapRunErr adds context to a RunJob/Watch error without adding another
// branch to the caller, which is already at cyclop's per-function
// complexity budget.
func wrapRunErr(err error) error {
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
}

// statePath returns the sqlite database path for pipeline's persisted job
// state: under .steps/ beside the pipeline YAML, named for the FILE.
//
// Per file, not per directory. Everything in the database is keyed by a name
// the pipeline chose for itself — job, resource, queue row — so two pipelines
// in one folder are two namespaces, not one, and a shared `.steps/state.db`
// silently merged them: one pipeline's version change could enqueue a job the
// other then claimed and ran, resource version history accumulated under one
// name for both, and `steps web app.yml infra.yml` handed the same store to
// two drainers (defeating the SetMaxOpenConns(1) serialization store.go
// relies on). That layout is also the one README and docs/web.md advertise.
//
// There is no migration, per this repo's no-migration rule: an older
// `.steps/state.db` is simply not read any more. The first run after this
// re-seeds its cold start, which triggers nothing — the safe direction to be
// wrong in.
func statePath(pipeline string) string {
	return filepath.Join(filepath.Dir(pipeline), ".steps", filepath.Base(pipeline)+".db")
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

// applyPreflightFlag threads --no-preflight down to internal/pipeline.
func applyPreflightFlag(ctx context.Context, skip bool) context.Context {
	if !skip {
		return ctx
	}

	return pipeline.WithoutPreflight(ctx)
}

// PreflightCmd checks that a job's models and MCP servers are live, and runs
// nothing.
//
// It is the same probe `steps run` performs automatically, exposed as a verb
// for the case where you want the answer without committing to the run —
// "before I kick this off for an hour, is the model up?". Pairing it with
// `steps validate` covers both halves: validate answers "is this pipeline
// runnable at all", preflight answers "is it runnable right now".
type PreflightCmd struct {
	Pipeline string            `arg:""                                                     help:"path to the pipeline YAML file"`
	Job      string            `help:"job to check (defaults to the pipeline's only job)"`
	Var      map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..." name:"var"`
	VarsFile string            `help:"YAML file of pipeline vars"                          name:"vars-file"`
}

// Run probes every model and MCP server the target job reaches and prints one
// line per target.
func (p *PreflightCmd) Run() error {
	cfg, err := loadWithVars(p.Pipeline, p.Var, p.VarsFile)
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, p.Job)
	if err != nil {
		return err
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	problems := pipeline.Preflight(ctx, cfg, job)
	if len(problems) > 0 {
		return fmt.Errorf("job %q cannot run right now:\n%s", job.Name, renderProblems(problems))
	}

	fmt.Printf("ok: job %q — every model and MCP server it needs responded\n", job.Name)

	return nil
}

// JobsCmd inspects and clears the watch circuit breaker.
//
// It exists because a paused job is otherwise invisible: `steps watch` stops
// triggering it and says so once, in output that has long since scrolled past
// by the time anyone wonders why the nightly summary stopped arriving.
type JobsCmd struct {
	Pipeline string `arg:""                                     help:"path to the pipeline YAML file"`
	Resume   string `help:"job to take out of the paused state"`
}

// Run lists paused jobs, or resumes one.
func (j *JobsCmd) Run() error {
	cfg, err := config.LoadConfig(j.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	st, _, cleanup, err := setup(cfg, j.Pipeline, false)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := context.Background()

	if j.Resume != "" {
		return resumeJob(ctx, cfg, st, j.Resume)
	}

	paused, err := st.PausedJobs(ctx)
	if err != nil {
		return fmt.Errorf("could not list paused jobs: %w", err)
	}

	if len(paused) == 0 {
		fmt.Println("no jobs are paused")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "JOB\tCONSECUTIVE FAILURES\tPAUSED AT")

	for _, job := range paused {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\n", job.Name, job.Consecutive, job.PausedAt)
	}

	err = writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the paused-job table: %w", err)
	}

	return nil
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
func loadWithVars(path string, flags map[string]string, varsFile string) (*config.Config, error) {
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

	cfg, err := config.LoadConfigWithVars(path, vars)
	if err != nil {
		return nil, fmt.Errorf("could not load pipeline: %w", err)
	}

	return cfg, nil
}

// WebCmd serves the pipeline UI.
//
// One or more pipeline files: state is per-pipeline by construction
// (.steps/state.db lives beside each YAML), so serving several means opening
// several stores, and the UI routes them under /p/<name>/.
//
// It polls trigger: true resources as well as serving, because a front end
// that drains a queue nothing fills is a runner that looks alive and notices
// nothing — the surprise this default exists to remove. --no-watch turns it
// off for someone who runs `steps watch` separately.
//
// It binds loopback by default and has no authentication, because there is
// nothing to authenticate against — this is the local runner's own front end,
// in the same trust domain as the shell that started it. Binding it to a
// routable address publishes trigger and approval controls to anyone who can
// reach the port; --listen exists for the person who has decided that is what
// they want, not as a default.
type WebCmd struct {
	Pipeline      []string          `arg:""                                                          help:"path(s) to pipeline YAML files"`
	Listen        string            `default:"127.0.0.1:8088"                                        help:"address to serve on"`
	Interval      time.Duration     `default:"30s"                                                   help:"how often to check trigger: true resources"`
	NoWatch       bool              `help:"serve without polling trigger: true resources"            name:"no-watch"`
	NoPreflight   bool              `help:"skip the pre-poll health check of models and MCP servers" name:"no-preflight"`
	ReadOnly      bool              `help:"serve without trigger, approval, or resume controls"      name:"read-only"`
	KeepWorkspace bool              `env:"STEPS_KEEP_WORKSPACE"                                      help:"leave build workspaces on disk instead of deleting them"`
	Var           map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..."      name:"var"`
	VarsFile      string            `help:"YAML file of pipeline vars"                               name:"vars-file"`
}

// Run loads every named pipeline, opens its store, and serves until canceled.
func (w *WebCmd) Run() error {
	// Rejected rather than shrugged at: `steps watch` refuses a non-positive
	// interval, and a web that quietly served forever without ever polling
	// would be the exact confusion this command's default exists to remove.
	if !w.NoWatch && w.Interval <= 0 {
		return fmt.Errorf("web: --interval must be positive, got %s; --no-watch is how polling is turned off", w.Interval)
	}

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx = applyPreflightFlag(ctx, w.NoPreflight)

	pipelines, providers, cleanup, err := w.load()
	if err != nil {
		return err
	}
	defer cleanup()

	// Tried for every pipeline, even under --no-watch: the lock guards more
	// than polling. See claimWatchLocks.
	claims := claimWatchLocks(pipelines)
	defer releaseWatchLocks(claims)

	// Before either loop below exists, because ResetStaleRunning is only safe
	// with no concurrent writer — see web.PrepareQueue.
	for i, target := range pipelines {
		web.PrepareQueue(ctx, target, claims[i].owned)
	}

	// Held only long enough to decide whether that recovery was ours to do:
	// keeping it would block the `steps watch` this flag exists to defer to.
	if w.NoWatch {
		releaseWatchLocks(claims)
	}

	var runner web.Runner

	local := web.NewLocalRunner(providers)
	if !w.ReadOnly {
		runner = local
	}

	server, err := web.New(pipelines, runner)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	var background sync.WaitGroup

	// Registered after `defer cleanup()`, so it runs BEFORE it: both loops
	// below write through stores cleanup closes. cancel() here rather than
	// relying on the deferred one at the top, since Start also returns on its
	// own error, with the context still live and the loops still running.
	defer func() {
		cancel()
		background.Wait()
	}()

	// The drainer runs regardless of --read-only: a row queued by a separate
	// `steps watch` against the same database is still work this process can
	// do. What --read-only withholds is the UI's ability to ADD work.
	background.Add(1)

	go func() {
		defer background.Done()

		local.Drain(ctx, pipelines)
	}()

	w.startPolling(ctx, &background, pipelines, claims)

	fmt.Printf("steps web: http://%s\n", w.Listen)

	err = server.Start(ctx, w.Listen)
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	return nil
}

// startPolling launches one trigger poller per served pipeline, so this
// process both fills and drains the queue — unless --no-watch, which leaves
// the filling to a separate `steps watch`.
//
// Each poller is handed its pipeline's OWN store handle rather than opening a
// second one; trigger.Poll's doc comment says why, and is the one copy of
// that reasoning.
//
// --read-only does not disable polling, deliberately: it withholds the
// BROWSER's ability to add work, which is a statement about the HTTP surface,
// not about what this process does on its own. `--listen 0.0.0.0 --read-only`
// is a build box that still has to notice new versions.
func (w *WebCmd) startPolling(
	ctx context.Context, background *sync.WaitGroup, pipelines []*web.Pipeline, claims []pipelineWatch,
) {
	if w.NoWatch {
		fmt.Println("steps web: not polling (--no-watch)")

		return
	}

	for i, target := range pipelines {
		// Said per pipeline, not counted up: a banner that reports "polling 3
		// pipelines" while two of them gave up is worse than no banner, and
		// which ones gave up is the part an operator needs.
		switch {
		case !claims[i].owned:
			fmt.Printf("steps web: %s is watched by another process; serving it without polling\n", target.Slug)
			slog.Info("web.poll_lock_held", "pipeline", target.Slug)

			continue
		case len(trigger.Resources(target.Cfg)) == 0:
			// Not a failure: plenty of pipelines are run by hand, and the UI
			// is exactly where you would run them from.
			fmt.Printf("steps web: %s has no trigger: true get; serving it without polling\n", target.Slug)

			continue
		}

		fmt.Printf("steps web: polling %s every %s\n", target.Slug, w.Interval)

		background.Add(1)

		go func() {
			defer background.Done()

			err := trigger.Poll(ctx, target.Cfg, target.Store, w.Interval)
			if err != nil && !errors.Is(err, trigger.ErrNoTriggers) {
				slog.Error("web.poll_stopped", "pipeline", target.Slug, "error", err)
			}
		}()
	}
}

// pipelineWatch is what this process learned when it tried to take a
// pipeline's single-watcher lock: whether it owns it, and how to give it back.
type pipelineWatch struct {
	owned   bool
	release func()
}

// claimWatchLocks tries the single-watcher lock for every served pipeline.
//
// It runs even under --no-watch, because the lock guards two things, not one.
// Polling is the obvious one — two pollers against a state.db claim each
// other's work. The other is RECOVERY: re-queueing stranded rows reads every
// running row as an abandoned leftover, which is only true when no other
// watcher is alive, and `steps web` next to `steps watch` is a pairing this
// command's docs actively recommend.
//
// A held lock is not fatal here, unlike in watch: serving is this command's
// job and polling is the extra, so it gives up that pipeline's polling and
// keeps serving it — and keeps the other pipelines' polling.
func claimWatchLocks(pipelines []*web.Pipeline) []pipelineWatch {
	claims := make([]pipelineWatch, len(pipelines))

	for i, target := range pipelines {
		release, held, err := target.Store.AcquireWatchLock()

		switch {
		case err != nil:
			slog.Error("web.watch_lock", "pipeline", target.Slug, "error", err)
		case held:
			slog.Info("web.watch_lock_held", "pipeline", target.Slug)
		default:
			// Once, so the --no-watch early release and the deferred one at
			// shutdown are not two closes of the same lock file.
			claims[i] = pipelineWatch{owned: true, release: sync.OnceFunc(release)}
		}
	}

	return claims
}

func releaseWatchLocks(claims []pipelineWatch) {
	for _, claim := range claims {
		if claim.release != nil {
			claim.release()
		}
	}
}

// load opens every pipeline named on the command line, along with its store,
// workspace provider, and event bus.
func (w *WebCmd) load() ([]*web.Pipeline, map[string]workspace.Provider, func(), error) {
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
		cfg, err := loadWithVars(path, w.Var, w.VarsFile)
		if err != nil {
			cleanup()

			return nil, nil, nil, err
		}

		st, provider, closeOne, err := setup(cfg, path, w.KeepWorkspace)
		if err != nil {
			cleanup()

			return nil, nil, nil, err
		}

		slug := web.Slugify(path)
		bus := events.New(pipeline.StoreSink(st))

		// Bus first, store second: cleanup runs these in order, and the bus
		// drains its queued events INTO the store. Closing the store first
		// would throw away the tail of whatever run was in flight.
		closers = append(closers, bus.Close, closeOne)

		providers[slug] = provider
		pipelines = append(pipelines, &web.Pipeline{
			Slug: slug, Path: path, Cfg: cfg, Store: st, Bus: bus,
		})
	}

	return pipelines, providers, cleanup, nil
}

// ApprovalsCmd lists approval: steps waiting for a decision.
//
// A parked approval that nobody is told about is useless in practice, so this
// is the "what is waiting on me?" command. It reads the same rows the audit
// trail is made of.
type ApprovalsCmd struct {
	Pipeline string `arg:"" help:"path to the pipeline YAML file"`
}

// Run prints every pending approval.
func (a *ApprovalsCmd) Run() error {
	st, cleanup, err := openStore(a.Pipeline)
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
	Pipeline string `arg:""                                       help:"path to the pipeline YAML file"`
	ID       int64  `arg:""                                       help:"the approval id, from steps approvals"`
	Reason   string `help:"note to record alongside the decision"`
}

// Run approves the named approval.
func (a *ApproveCmd) Run() error {
	return decideApproval(a.Pipeline, a.ID, "approved", a.Reason)
}

// RejectCmd records a no.
type RejectCmd struct {
	Pipeline string `arg:""                                    help:"path to the pipeline YAML file"`
	ID       int64  `arg:""                                    help:"the approval id, from steps approvals"`
	Reason   string `help:"why — recorded with the decision"`
}

// Run rejects the named approval.
func (r *RejectCmd) Run() error {
	return decideApproval(r.Pipeline, r.ID, "rejected", r.Reason)
}

// decideApproval records a decision against a pipeline's store.
func decideApproval(pipelinePath string, id int64, status, reason string) error {
	st, cleanup, err := openStore(pipelinePath)
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

// openStore opens a pipeline's state store without building any workspace —
// the read-only path the approval commands need.
func openStore(pipelinePath string) (*store.Store, func(), error) {
	cfg, err := config.LoadConfig(pipelinePath)
	if err != nil {
		return nil, nil, fmt.Errorf("could not load pipeline: %w", err)
	}

	st, _, cleanup, err := setup(cfg, pipelinePath, false)
	if err != nil {
		return nil, nil, err
	}

	return st, cleanup, nil
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
func (r *RunsCmd) printCostTotals(ctx context.Context, st *store.Store) error {
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

	fmt.Printf("\nbreak one down with: steps runs %s --cost --run <id>\n", r.Pipeline)

	return nil
}

// printRunCost breaks one run down per agent step.
func (r *RunsCmd) printRunCost(ctx context.Context, st *store.Store) error {
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
// Never $0.00 for an unreported cost: no provider path reports dollars today,
// so a zero here would say every run was free rather than that nobody priced
// it. See the agent_usage schema comment.
func renderCost(cost *float64, unpriced int) string {
	if cost == nil {
		return "unpriced"
	}

	rendered := fmt.Sprintf("$%.2f", *cost)
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
