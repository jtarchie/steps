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
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/alecthomas/kong"
	"github.com/lmittmann/tint"

	"github.com/jtarchie/steps/internal/blobstore"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
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
	Pipeline  PipelineCmd      `cmd:""                                  help:"tell a steps web daemon which pipelines to serve"`
	Docs      DocsCmd          `cmd:""                                  help:"read the docs in the terminal (no page name lists them)"`
	// Last, and hidden: see ShimCmd. Placing it here keeps the help ordering
	// of the real commands untouched.
	Shim ShimCmd `cmd:"" hidden:"" name:"_shim"`
}

// BuildVersion is the version string steps --version prints. Overridden at
// build time via -ldflags "-X github.com/jtarchie/steps/internal/cli.BuildVersion=...";
// "dev" covers `go run`/unversioned `go build` invocations.
var BuildVersion = "dev"

// RecordRevision writes down WHICH configuration the runs opened against this
// handle will have executed.
//
// Called from the same funnel as SetSourcePath, and for the same reason: it
// is the only place holding both the configuration that was parsed and the
// handle its runs are recorded against. A command that only READS history
// never comes through there and records no revision, which is right — it
// resolved no configuration.
func RecordRevision(ctx context.Context, st store.Revisions, cfg *config.Config) error {
	if !cfg.Revision.Recorded() {
		return nil
	}

	err := st.RecordRevision(ctx, cfg.Revision.SHA, cfg.Revision.Source, cfg.Revision.Includes)
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
	slog.SetDefault(slog.New(events.LogHandler(tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:     parseLogLevel(level),
		AddSource: true,
		NoColor:   wantNoColor(),
	}))))
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
) (store.Store, workspace.Provider, func(), error) {
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

	st, err := sqlite.OpenStore(StatePath(pipelinePath, flags.DB), name)
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
func attachArtifactStore(provider workspace.Provider, st store.Blobs, raw string) error {
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
			slog.WarnContext(ctx, "signal.received", "signal", sig.String())
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
