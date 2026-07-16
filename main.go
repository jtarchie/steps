// Package main implements steps, a small CLI that interprets a
// Concourse-style pipeline YAML file (resource_types/resources/jobs):
// check discovers resource versions, get fetches one via a rendered
// shell command, and task runs a plan step's command. `run` executes one
// job once; `watch` polls trigger: true resources and auto-runs every job
// a changed resource affects.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/lmittmann/tint"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/trigger"
	"github.com/jtarchie/steps/internal/workspace"
)

// CLI is the pipeline runner's command-line grammar, parsed by kong. Run is
// default:"withargs" so today's flat invocation (steps pipeline.yml --job x)
// keeps working unchanged, routed to it implicitly.
type CLI struct {
	Run   RunCmd   `cmd:"" default:"withargs"                                             help:"run a single job once"`
	Watch WatchCmd `cmd:"" help:"poll trigger: true resources and auto-run affected jobs"`
}

// RunCmd runs a single job's plan once, exactly as steps has always done.
type RunCmd struct {
	Pipeline string            `arg:""                                                                 help:"path to the pipeline YAML file"`
	Job      string            `help:"job name to run (defaults to the pipeline's only job)"`
	Version  map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"`
	Force    bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
}

// Run loads the pipeline, selects a job, and runs it once via
// pipeline.RunJob.
func (r *RunCmd) Run() error {
	cfg, err := config.LoadConfig(r.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	job, err := selectJob(cfg, r.Job)
	if err != nil {
		return err
	}

	st, provider, cleanup, err := setup(cfg, r.Pipeline)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	return wrapRunErr(pipeline.RunJob(ctx, cfg, job, r.Version, provider, st, r.Force))
}

// WatchCmd polls every resource named by a trigger:true get step, across
// every job in the pipeline, and runs whichever jobs a version change
// affects — see internal/trigger.
type WatchCmd struct {
	Pipeline      string            `arg:""                                                                 help:"path to the pipeline YAML file"`
	Interval      time.Duration     `default:"30s"                                                          help:"how often to check trigger: true resources"`
	MaxConcurrent int               `default:"1"                                                            help:"maximum number of triggered jobs running at once"`
	Version       map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"`
	Force         bool              `help:"ignore persisted state and re-run every step, even if unchanged"`
}

// Run loads the pipeline and blocks in trigger.Watch until canceled
// (SIGINT/SIGTERM).
func (w *WatchCmd) Run() error {
	cfg, err := config.LoadConfig(w.Pipeline)
	if err != nil {
		return fmt.Errorf("could not load pipeline: %w", err)
	}

	st, provider, cleanup, err := setup(cfg, w.Pipeline)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := withSignalCancel(context.Background())
	defer cancel()

	return wrapRunErr(trigger.Watch(ctx, cfg, provider, st, w.Version, w.Interval, w.MaxConcurrent, w.Force))
}

// initLogging installs a debug-level slog handler on stderr as the default
// logger, separate from this tool's plain stdout progress lines
// ("get: prs (version: ...)", "task: review").
func initLogging() {
	slog.SetDefault(slog.New(tint.NewTextHandler(os.Stderr, &tint.Options{
		Level:     slog.LevelDebug,
		AddSource: true,
	})))
}

func main() {
	initLogging()

	err := run(os.Args[1:])
	if err != nil {
		slog.Error("main.run", "error", err)
		fmt.Fprintf(os.Stderr, "steps: error: %v\n", err)
		os.Exit(1)
	}

	slog.Debug("main.exit", "code", 0)
}

func run(args []string) error {
	slog.Debug("cli.parse", "args", args)

	var cli CLI

	parser, err := kong.New(&cli, kong.Name("steps"), kong.Description("run pipeline jobs, or watch for trigger: true resource changes"))
	if err != nil {
		return fmt.Errorf("could not build CLI parser: %w", err)
	}

	kctx, err := parser.Parse(args)
	if err != nil {
		return fmt.Errorf("could not parse flags: %w", err)
	}

	return kctx.Run() //nolint:wrapcheck // the Run methods above already wrap their own errors via wrapRunErr
}

// setup opens the state store and builds/validates the workspace provider
// shared by RunCmd and WatchCmd, returning a cleanup func that closes both
// (logging, not returning, any close error — mirroring the deferred
// close-error handling both commands used inline before this helper
// existed).
func setup(cfg *config.Config, pipelinePath string) (*store.Store, workspace.Provider, func(), error) {
	st, err := store.OpenStore(statePath(pipelinePath))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not open state store: %w", err)
	}

	provider, err := workspace.NewProvider(cfg.Workspace)
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
