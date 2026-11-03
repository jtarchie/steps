// Package cli implements steps' command-line grammar and every command
// behind it. `run` executes one job once and `test` every job; `web` is the
// daemon that holds the pipelines `pipeline set` uploads, serves the UI, polls
// trigger: true resources and auto-runs every job a changed resource affects.
//
// It lives below main rather than in it so the end-to-end suite in ./e2e can
// drive the whole stack through Run, which is the only entry point that
// spans it.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/alecthomas/kong"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/telemetry"
)

// CLI is the pipeline runner's command-line grammar, parsed by kong. Run is
// default:"withargs" so today's flat invocation (steps pipeline.yml --job x)
// keeps working unchanged, routed to it implicitly. LogLevel is a global flag
// (available to every subcommand, not just Run) rather than living on
// RunCmd/TestCmd individually, since it configures the process-wide slog
// default logger before any subcommand's Run method executes — see
// InitLogging.
type CLI struct {
	LogLevel       string            `default:"info"                          enum:"debug,info,warn,error"                                             env:"STEPS_LOG_LEVEL"        help:"log verbosity: debug, info, warn, or error"`
	Version        kong.VersionFlag  `help:"print the steps version and exit" name:"version"`
	Run            RunCmd            `cmd:""                                  default:"withargs"                                                       help:"run a single job once"`
	Test           TestCmd           `cmd:""                                  help:"run every job (force) and verify assert directives"`
	Validate       ValidateCmd       `cmd:""                                  help:"check a pipeline for errors without running anything"`
	Runs           RunsCmd           `cmd:""                                  help:"show what past runs recorded"`
	Plan           PlanCmd           `cmd:""                                  help:"show which steps a run would execute or skip"`
	MCP            MCPCmd            `cmd:""                                  help:"inspect or authorize a pipeline's mcp_servers: entries"`
	Jobs           JobsCmd           `cmd:""                                  help:"jobs the circuit breaker has paused, and taking one out of it"`
	Approvals      ApprovalsCmd      `cmd:""                                  help:"approval: steps waiting for a decision, and deciding them"`
	Questions      QuestionsCmd      `cmd:""                                  help:"ask_user questions waiting for an answer, and answering them"`
	Memory         MemoryCmd         `cmd:""                                  help:"what agent steps remember between runs, and deleting it"`
	ClearTaskCache ClearTaskCacheCmd `cmd:""                                  help:"empty a step's caches: so its next run starts cold"`
	Web            WebCmd            `cmd:""                                  help:"serve the UI, poll trigger: true resources, and run affected jobs"`
	Pipeline       PipelineCmd       `cmd:""                                  help:"tell a steps web daemon which pipelines to serve"`
	Docs           DocsCmd           `cmd:""                                  help:"read the docs in the terminal (no page name lists them)"`
}

// BuildVersion is the version string steps --version prints. Overridden at
// build time via -ldflags "-X github.com/jtarchie/steps/internal/cli.BuildVersion=...";
// "dev" covers `go run`/unversioned `go build` invocations.
var BuildVersion = "dev"

// version is BuildVersion, or what the go command recorded for an unstamped build: `go install github.com/jtarchie/steps@v0.1.0` passes no ldflags, and printed "dev".
func version() string { return moduleVersion(BuildVersion, debug.ReadBuildInfo) }

func moduleVersion(stamped string, read func() (*debug.BuildInfo, bool)) string {
	if stamped != "dev" {
		return stamped
	}

	info, ok := read()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return stamped
	}

	return info.Main.Version
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

// Run parses args as the steps command line and executes the command they
// name. It is the whole CLI behind one call, which is what lets the
// end-to-end suite drive a real invocation in-process rather than spawning
// a binary.
func Run(args []string) error {
	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Description("run pipeline jobs, or serve and poll them with steps web"), kong.Vars{"version": version()})
	if err != nil {
		return fmt.Errorf("could not build CLI parser: %w", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return fmt.Errorf("could not parse flags: %w", err)
	}

	InitLogging(cli.LogLevel)

	for _, flag := range kctx.Flags() {
		if db, ok := flag.Target.Interface().(DB); ok {
			warnPasswordInURL(string(db))
		}
	}

	slog.Debug("cli.parse", "command", kctx.Command(), "args", len(args))

	flush, err := telemetry.Start(context.Background(), version())
	if err != nil {
		return err //nolint:wrapcheck // telemetry already says it is telemetry
	}
	defer flush(context.Background())

	return kctx.Run() //nolint:wrapcheck // the Run methods above already wrap their own errors via wrapRunErr
}

// wrapRunErr adds context to a RunJob error without adding another
// branch to the caller, which is already at cyclop's per-function
// complexity budget.
func wrapRunErr(err error) error {
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}

	return nil
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
