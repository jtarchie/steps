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
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/alecthomas/kong"
	"github.com/lmittmann/tint"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/trigger"
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
}

// buildVersion is the version string steps --version prints. Overridden at
// build time via -ldflags "-X main.buildVersion=...";  "dev" covers `go run`/
// unversioned `go build` invocations.
var buildVersion = "dev"

// RunCmd runs a single job's plan once, exactly as steps has always done.
type RunCmd struct {
	Pipeline      string            `arg:""                                                                   help:"path to the pipeline YAML file"`
	Job           string            `help:"job name to run (defaults to the pipeline's only job)"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                  name:"pin"`
	Force         bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	KeepWorkspace bool              `env:"STEPS_KEEP_WORKSPACE"                                               help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight   bool              `help:"skip the pre-run health check of the job's models and MCP servers" name:"no-preflight"`
	Var           map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..."               name:"var"`
	VarsFile      string            `help:"YAML file of pipeline vars"                                        name:"vars-file"`
}

// Run loads the pipeline, selects a job, and runs it once via
// pipeline.RunJob.
func (r *RunCmd) Run() error {
	cfg, err := loadWithVars(r.Pipeline, r.Var, r.VarsFile)
	if err != nil {
		return err
	}

	job, err := selectJob(cfg, r.Job)
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

	ctx = applyPreflightFlag(ctx, r.NoPreflight)

	runErr := pipeline.RunJob(ctx, cfg, job, r.Pin, provider, st, r.Force)

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
	Pipeline      string            `arg:""                                                                    help:"path to the pipeline YAML file"`
	Interval      time.Duration     `default:"30s"                                                             help:"how often to check trigger: true resources"`
	MaxConcurrent int               `default:"1"                                                               help:"maximum number of triggered jobs running at once"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                   name:"pin"`
	Force         bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
	KeepWorkspace bool              `env:"STEPS_KEEP_WORKSPACE"                                                help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight   bool              `help:"skip the pre-run health check of each job's models and MCP servers" name:"no-preflight"`
	Listen        string            `help:"serve webhook checks on this address, e.g. :8080"                   name:"listen"`
}

// Run loads the pipeline and blocks in trigger.Watch until canceled
// (SIGINT/SIGTERM).
func (w *WatchCmd) Run() error {
	cfg, err := config.LoadConfig(w.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	st, provider, cleanup, err := setup(cfg, w.Pipeline, w.KeepWorkspace)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	ctx = applyPreflightFlag(ctx, w.NoPreflight)

	return wrapRunErr(trigger.Watch(ctx, cfg, provider, st, w.Pin, w.Interval, w.MaxConcurrent, w.Force, w.Listen))
}

// TestCmd runs every job in the pipeline (force, so nothing is skipped and the
// recorded execution order is deterministic) and verifies its assert:
// directives — each job's own assert.execution is checked inside RunJob, and a
// top-level assert.execution of job names is checked here. It's the entry
// point for a self-verifying fixture (see examples/flow.yml).
type TestCmd struct {
	Pipeline string `arg:"" help:"path to the pipeline YAML file"`
}

// Run loads the pipeline, runs every job (force), and reports pass/fail per
// job plus the pipeline-level assert.execution. It returns a non-nil error if
// any job failed or the pipeline assert mismatched, so the process exits
// non-zero.
func (t *TestCmd) Run() error {
	cfg, err := config.LoadConfig(t.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
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

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]
		executed = append(executed, job.Name)

		jobErr := pipeline.RunJob(ctx, cfg, job, nil, provider, st, true)
		if jobErr != nil {
			fmt.Printf("FAIL %s: %v\n", job.Name, jobErr)

			failures = append(failures, job.Name)

			continue
		}

		fmt.Printf("PASS %s\n", job.Name)
	}

	if cfg.Assert != nil && len(cfg.Assert.Execution) > 0 && !slices.Equal(cfg.Assert.Execution, executed) {
		return fmt.Errorf("pipeline assert.execution mismatch:\n  want: %v\n  got:  %v", cfg.Assert.Execution, executed)
	}

	if len(failures) > 0 {
		return fmt.Errorf("test: %d job(s) failed: %v", len(failures), failures)
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
	SyntaxOnly bool `help:"check the file only; skip credential and MCP-binary checks about this machine" name:"syntax-only"`
}

// Run loads the pipeline (which runs every config-level validator) and then
// checks artifact flow for each job, joining the failures so one invocation
// reports everything wrong with the file.
func (v *ValidateCmd) Run() error {
	cfg, err := config.LoadConfig(v.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	var errs []error

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
	Pipeline string            `arg:""                                                   help:"path to the pipeline YAML file"`
	Job      string            `help:"job to plan (defaults to the pipeline's only job)"`
	Pin      map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"  name:"pin"`
}

// Run loads the pipeline, plans the selected job, and prints one line per
// step. Resource check commands run (planning has always resolved get
// versions), but no step executes and nothing is recorded.
func (p *PlanCmd) Run() error {
	cfg, err := config.LoadConfig(p.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
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
	Tools MCPToolsCmd `cmd:"" help:"list an mcp server's tools and their argument schemas"`
	Login MCPLoginCmd `cmd:"" help:"interactively authorize an oauth-configured mcp server"`
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
// (internal/mcp's loopbackCallback.fetch) already falls back to printing
// url to stdout on any error this returns, so this only needs to cover the
// happy path per OS — a nil case (no known opener for GOOS) fails closed
// into that same print-the-URL fallback rather than guessing.
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
// state: .steps/state.db colocated with the pipeline YAML's own directory,
// so distinct pipelines never share a database file.
func statePath(pipeline string) string {
	return filepath.Join(filepath.Dir(pipeline), ".steps", "state.db")
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

	return ctx, cancel
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
	Pipeline string `arg:""                                                    help:"path to the pipeline YAML file"`
	Job      string `help:"job to check (defaults to the pipeline's only job)"`
}

// Run probes every model and MCP server the target job reaches and prints one
// line per target.
func (p *PreflightCmd) Run() error {
	cfg, err := config.LoadConfig(p.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
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
