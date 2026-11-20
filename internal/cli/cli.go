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
	"io"
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/alecthomas/kong"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
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
	Fmt            FmtCmd            `cmd:""                                  help:"rewrite pipeline files into the standard layout"`
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
	return RunWith(Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}, args)
}

// Streams is where one invocation reads what a person types and writes what a person reads. The process's own streams are only ever named in Run, so two invocations in one process (the end-to-end suite runs hundreds side by side) each write to their own.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
	// stop is RunContext's ctx.Done(), nil under Run and RunWith.
	stop <-chan struct{}
	// logLevel is the invocation's --log-level, for the handler a stderr of its own gets.
	logLevel string
}

// context is a background context whose step output and notes go to these streams, where nothing installed means the process's own.
func (s Streams) context() context.Context {
	return s.logs(events.WithOutput(context.Background(), events.Output{Stdout: s.Out, Stderr: s.Err}))
}

// logs is ctx with its log lines going to a stderr of the invocation's own, when it was handed one.
func (s Streams) logs(ctx context.Context) context.Context {
	if s.Err == nil || s.Err == io.Writer(os.Stderr) {
		return ctx
	}

	return withLogs(ctx, s.Err, s.logLevel)
}

// cancellable is context() cancelled by the first SIGINT or SIGTERM, or by RunContext's ctx ending — the same stop, delivered to this invocation alone.
func (s Streams) cancellable() (context.Context, context.CancelFunc) {
	ctx, cancel := withSignalCancel(s.context())

	if s.stop != nil {
		go func() {
			select {
			case <-s.stop:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	return ctx, cancel
}

// RunWith is Run writing to streams instead of the process's stdout and stderr. Log lines follow when Err is not the process's own: slog has one handler per process, and it routes a record by the context it was logged with (see withLogs).
func RunWith(streams Streams, args []string) error {
	return RunContext(context.Background(), streams, args)
}

// RunContext is RunWith for a command something other than a signal may stop: cancelling ctx is the first SIGINT, to this invocation alone. A daemon under test is stopped this way, since a signal goes to every invocation in the process.
func RunContext(ctx context.Context, streams Streams, args []string) error {
	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Description("run pipeline jobs, or serve and poll them with steps web"), kong.Vars{"version": version()}, kong.Writers(streams.Out, streams.Err))
	if err != nil {
		return fmt.Errorf("could not build CLI parser: %w", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return fmt.Errorf("could not parse flags: %w", err)
	}

	InitLogging(cli.LogLevel)

	streams.stop = ctx.Done()
	streams.logLevel = cli.LogLevel
	// What this invocation logs before a command has a context of its own.
	logs := streams.logs(ctx)

	for _, flag := range kctx.Flags() {
		if db, ok := flag.Target.Interface().(DB); ok {
			warnPasswordInURL(logs, string(db))
		}
	}

	slog.DebugContext(logs, "cli.parse", "command", kctx.Command(), "args", len(args))

	// Telemetry outlives the stop: the spans of a cancelled run are the ones worth exporting.
	flush, err := telemetry.Start(context.WithoutCancel(ctx), version())
	if err != nil {
		return err //nolint:wrapcheck // telemetry already says it is telemetry
	}
	defer flush(context.WithoutCancel(ctx))

	return kctx.Run(streams) //nolint:wrapcheck // the Run methods above already wrap their own errors via wrapRunErr
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
