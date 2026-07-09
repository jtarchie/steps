package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// versionFlagList implements flag.Value, accumulating "key=value" pairs
// from repeated --version flags into a map[string]string.
type versionFlagList map[string]string

func (v versionFlagList) String() string {
	parts := make([]string, 0, len(v))
	for k, val := range v {
		parts = append(parts, k+"="+val)
	}

	return strings.Join(parts, ",")
}

func (v versionFlagList) Set(s string) error {
	key, value, found := strings.Cut(s, "=")
	if !found || key == "" {
		return fmt.Errorf("invalid --version flag %q: expected key=value", s)
	}

	v[key] = value

	return nil
}

// splitPositional pulls the first non-flag token out of args as the
// positional pipeline path, returning it along with the remaining tokens
// (in original order) for flag parsing.
func splitPositional(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)

			return a, rest
		}
	}

	return "", args
}

// initLogging installs a debug-level slog handler on stderr as the default
// logger, separate from this tool's plain stdout progress lines
// ("get: prs (version: ...)", "task: review").
func initLogging() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
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

	// Go's flag package stops parsing flags at the first non-flag token, so
	// the positional pipeline path must be pulled out before handing the
	// rest to fs.Parse — otherwise "--job"/"--version" after it would never
	// be recognized as flags.
	pipelinePath, flagArgs := splitPositional(args)
	if pipelinePath == "" {
		err := errors.New("usage: steps <pipeline.yml> [--job NAME] [--version key=value]...")
		slog.Error("cli.parse", "error", err)

		return err
	}

	fs := flag.NewFlagSet("steps", flag.ContinueOnError)

	jobName := fs.String("job", "", "job name to run (defaults to the pipeline's only job)")

	pinned := make(versionFlagList)
	fs.Var(&pinned, "version", "pin a version field, e.g. number=87 (repeatable)")

	err := fs.Parse(flagArgs)
	if err != nil {
		slog.Error("cli.parse", "flag_args", flagArgs, "error", err)

		return err
	}

	slog.Debug("cli.parsed", "pipeline", pipelinePath, "job", *jobName, "pinned", map[string]string(pinned))

	cfg, err := LoadConfig(pipelinePath)
	if err != nil {
		slog.Error("cli.load_config", "path", pipelinePath, "error", err)

		return err
	}

	name := *jobName
	if name == "" {
		if len(cfg.Jobs) != 1 {
			names := make([]string, 0, len(cfg.Jobs))
			for _, j := range cfg.Jobs {
				names = append(names, j.Name)
			}

			err := fmt.Errorf("--job is required when the pipeline has more than one job (available: %v)", names)
			slog.Error("cli.select_job", "available", names, "error", err)

			return err
		}

		name = cfg.Jobs[0].Name
		slog.Debug("cli.select_job", "job", name, "reason", "only job in pipeline")
	}

	job, err := cfg.FindJob(name)
	if err != nil {
		slog.Error("cli.select_job", "job", name, "error", err)

		return err
	}

	workspaceDir, err := os.MkdirTemp("", "steps-*")
	if err != nil {
		err = fmt.Errorf("could not create workspace: %w", err)
		slog.Error("workspace.create", "error", err)

		return err
	}

	slog.Debug("workspace.create", "dir", workspaceDir)

	defer func() {
		slog.Debug("workspace.remove", "dir", workspaceDir)

		err := os.RemoveAll(workspaceDir)
		if err != nil {
			slog.Error("workspace.remove", "dir", workspaceDir, "error", err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	return RunJob(ctx, cfg, job, pinned, workspaceDir)
}
