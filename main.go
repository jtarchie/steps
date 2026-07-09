package main

import (
	"context"
	"flag"
	"fmt"
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

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "steps: error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	// Go's flag package stops parsing flags at the first non-flag token, so
	// the positional pipeline path must be pulled out before handing the
	// rest to fs.Parse — otherwise "--job"/"--version" after it would never
	// be recognized as flags.
	pipelinePath, flagArgs := splitPositional(args)
	if pipelinePath == "" {
		return fmt.Errorf("usage: steps <pipeline.yml> [--job NAME] [--version key=value]...")
	}

	fs := flag.NewFlagSet("steps", flag.ContinueOnError)

	jobName := fs.String("job", "", "job name to run (defaults to the pipeline's only job)")

	pinned := make(versionFlagList)
	fs.Var(&pinned, "version", "pin a version field, e.g. number=87 (repeatable)")

	err := fs.Parse(flagArgs)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig(pipelinePath)
	if err != nil {
		return err
	}

	name := *jobName
	if name == "" {
		if len(cfg.Jobs) != 1 {
			names := make([]string, 0, len(cfg.Jobs))
			for _, j := range cfg.Jobs {
				names = append(names, j.Name)
			}

			return fmt.Errorf("--job is required when the pipeline has more than one job (available: %v)", names)
		}

		name = cfg.Jobs[0].Name
	}

	job, err := cfg.FindJob(name)
	if err != nil {
		return err
	}

	workspaceDir, err := os.MkdirTemp("", "steps-*")
	if err != nil {
		return fmt.Errorf("could not create workspace: %w", err)
	}
	defer os.RemoveAll(workspaceDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	return RunJob(ctx, cfg, job, pinned, workspaceDir)
}
