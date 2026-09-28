package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// RunCmd runs a single job's plan once, exactly as steps has always done.
type RunCmd struct {
	StateFlags    `embed:""`
	VarFlags      `embed:""`
	ExecFlags     `embed:""`
	HistoryFlags  `embed:""`
	ProgressFlags `embed:""`
	Pipeline      string            `arg:""                                                                                                                         help:"path to the pipeline YAML file"`
	Job           string            `help:"job name to run (defaults to the pipeline's only job)"`
	Pin           map[string]string `help:"pin a version field, e.g. number=87 (repeatable)"                                                                        name:"pin"`
	Force         bool              `help:"ignore the step cache and re-run every step, even if unchanged (version: every still takes only versions not yet built)"`
	Resume        string            `help:"continue a failed run from the step that failed"                                                                         name:"resume"`
	Replay        string            `help:"fork a recorded run and re-run it from --from onward"                                                                    name:"replay"`
	From          string            `help:"with --replay, the step name to re-run from"                                                                             name:"from"`
	Rerun         string            `help:"run a recorded run again against the versions it was created with: <run>, or one build of it with <run>#<build>"         name:"rerun"`
}

// applyContinuation handles the flags that point this invocation at a previous
// run, and reports which job to run.
//
// --resume resolves its job here; --replay only resolves its NAME here,
// because preparing it needs the job's plan to turn --from into a position —
// see applyReplay, which runs after selectJob.
func (r *RunCmd) applyContinuation(
	ctx context.Context, st store.Store, provider workspace.Provider, jobName string,
) (context.Context, string, error) {
	if r.Resume != "" && r.Replay != "" {
		return ctx, "", errors.New("--resume and --replay cannot be combined: one continues a failed run in place, the other forks a recorded one from a step you name")
	}

	var err error

	if r.Rerun != "" {
		return r.applyRerun(ctx, st)
	}

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

// applyRerun points this invocation at one build of a recorded run; the job is that run's.
func (r *RunCmd) applyRerun(ctx context.Context, st store.Store) (context.Context, string, error) {
	if r.Resume != "" || r.Replay != "" {
		return ctx, "", errors.New("--rerun cannot be combined with --resume or --replay: it starts a new run of one build from the top")
	}

	runID, build, err := parseRerun(r.Rerun)
	if err != nil {
		return ctx, "", err
	}

	ctx, jobName, err := pipeline.PrepareRerun(ctx, st, runID, build)
	if err != nil {
		return ctx, "", fmt.Errorf("could not rerun: %w", err)
	}

	return ctx, jobName, nil
}

// parseRerun reads <run>, every build of it, or <run>#<build>, one of them.
func parseRerun(value string) (string, int, error) {
	runID, index, found := strings.Cut(value, "#")
	if !found {
		return runID, -1, nil
	}

	build, err := strconv.Atoi(index)
	if err != nil || build < 0 {
		return "", 0, fmt.Errorf("--rerun %q: the build after # must be a number, as in %s#0", value, runID)
	}

	return runID, build, nil
}

// applyReplay forks a recorded run once the job is known.
func (r *RunCmd) applyReplay(
	ctx context.Context, st store.Store, provider workspace.Provider, cfg *config.Config, job *config.Job,
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

	ctx, err = r.prepare(ctx, cfg, st)
	if err != nil {
		return err
	}

	ctx = pipeline.WithAnswerDB(ctx, answerDB(r.Pipeline, r.DB))

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

	ctx, undraw := r.draw(ctx, st)
	runErr := pipeline.RunJob(ctx, cfg, job, r.Pin, provider, st, r.Force)

	undraw()

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
// recorded execution order is deterministic, and versions an every-get already took
// are re-opened so a rerun is too) and verifies its assert:
// directives — each job's own assert.execution is checked inside RunJob, and a
// top-level assert.execution of job names is checked here. It's the entry
// point for a self-verifying fixture — every runnable example in docs/*.md
// is one (see docs_test.go).
type TestCmd struct {
	StateFlags    `embed:""`
	VarFlags      `embed:""`
	ExecFlags     `embed:""`
	ProgressFlags `embed:""`
	Pipeline      string `arg:""   help:"path to the pipeline YAML file"`
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

	ctx, err = t.prepare(ctx, cfg, st)
	if err != nil {
		return err
	}

	ctx = pipeline.WithAnswerDB(ctx, answerDB(t.Pipeline, t.DB))
	ctx = pipeline.WithTakenVersionsReopened(ctx)

	var (
		executed []string
		failures []string
	)

	slog.Info("pipeline.test", "pipeline", t.Pipeline, "jobs", len(cfg.Jobs))

	ctx, undraw := t.draw(ctx, st)
	defer undraw()

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]
		executed = append(executed, job.Name)

		jobErr := pipeline.RunJob(ctx, cfg, job, nil, provider, st, true)
		if jobErr != nil {
			_, _ = fmt.Fprintf(events.Stdout(ctx), "FAIL %s: %v\n", job.Name, jobErr)

			// The REASON, not just the name. This error is what a caller
			// sees — a script, a CI step, or the mutation suite asking
			// which assertion caught a mutant — and "1 job(s) failed:
			// [build]" sends every one of them back to scrape stdout.
			failures = append(failures, fmt.Sprintf("  %s: %v", job.Name, jobErr))

			continue
		}

		_, _ = fmt.Fprintf(events.Stdout(ctx), "PASS %s\n", job.Name)
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

// applyResume points this invocation at a previous run: which steps it need
// not repeat, and which workspace to continue in.
//
// The job name comes from the recorded run rather than the flag, so
// `--resume <id>` alone is enough — asking an operator to remember which job a
// run id belonged to would make the id useless on its own.
func applyResume(
	ctx context.Context, st store.Store, provider workspace.Provider, runID, jobName string,
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
