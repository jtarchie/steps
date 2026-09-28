package cli

import (
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

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

	st, err := sqlite.OpenStore(StatePath(p.Pipeline, p.DB), resolvePipelineName(p.Pipeline, p.Name))
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
