package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/web"
)

// RunsCmd is what past runs recorded, in five views.
//
// The store has always written all of this and offered no way to read it: the
// only route to "why did my last run fail" was opening .steps/state.db in
// sqlite and knowing the schema, which the vendored pure-Go driver means may
// not even be installed.
//
// Subcommands rather than a flag switch, and the difference is not
// cosmetic: each view differs in what it needs NAMED. `list` reads one
// pipeline or every pipeline in a --db file; the other four are questions
// about one pipeline and cannot be anything else — a trigger queue belongs to
// a pipeline, a step's job name means nothing without one, and --run already
// refuses an id belonging to a neighbour in the same file. As flags on one
// command that distinction was a runtime table of which combinations to
// refuse; as subcommands it is the grammar, and kong enforces it.
type RunsCmd struct {
	List   RunsListCmd   `cmd:"" default:"withargs"                                                 help:"runs, newest first"`
	Steps  RunsStepsCmd  `cmd:"" help:"individual steps, with what each one recorded"`
	Queue  RunsQueueCmd  `cmd:"" help:"what the trigger loop has queued"`
	Cost   RunsCostCmd   `cmd:"" help:"what a pipeline's agent steps spent"`
	Where  RunsWhereCmd  `cmd:"" help:"the machines a run's placed steps ran on"`
	Abort  RunsAbortCmd  `cmd:"" help:"stop a run on a steps web daemon, or drop a queued one"`
	Follow RunsFollowCmd `cmd:"" help:"watch a run to its end: live on a terminal, lines elsewhere"`
}

// RunsListCmd is the default view: runs, newest first — and the one
// view that answers for a whole state file when no pipeline is named.
type RunsListCmd struct {
	ReadFlags `embed:""`
	Job       string `help:"only show runs of this job"`
	Limit     int    `default:"20"                      help:"maximum number of rows to show"`
}

// Run prints one pipeline's job runs, or every pipeline's in a shared file.
func (r *RunsListCmd) Run() error {
	// No pipeline named is the cross-pipeline question: one database holds however many a daemon was given, which is what the web root answers too.
	if r.Pipeline == "" {
		return r.runAcross()
	}

	if nothingRecorded(r.ReadFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printJobRuns(context.Background(), st)
}

// RunsStepsCmd is the per-step detail: what previous runs actually did.
//
// The run id is a positional for the reason RunsCostCmd's is: naming a run is
// asking for the deeper view — what that run completed, per build.
type RunsStepsCmd struct {
	ReadFlags `embed:""`
	RunID     string `arg:""                             help:"what this run completed, per build — the steps a --resume skips"   optional:""`
	Job       string `help:"only show steps of this job"`
	Limit     int    `default:"20"                       help:"maximum number of rows to show in the listing"`
}

// Run prints recorded steps, newest first, or what one run completed.
func (r *RunsStepsCmd) Run() error {
	// Refused rather than ignored: a run already names its job.
	if r.RunID != "" && r.Job != "" {
		return errors.New("--job does not apply to a named run; a run belongs to one job already")
	}

	if nothingRecorded(r.ReadFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	if r.RunID != "" {
		return r.printRunSteps(context.Background(), st)
	}

	return r.printSteps(context.Background(), st)
}

// RunsQueueCmd is what the trigger loop has queued and not yet run.
type RunsQueueCmd struct {
	ReadFlags `embed:""`
	Limit     int `default:"20" help:"maximum number of rows to show"`
}

// Run prints the trigger queue.
func (r *RunsQueueCmd) Run() error {
	if nothingRecorded(r.ReadFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printQueue(context.Background(), st)
}

// RunsCostCmd is what agent steps spent: per run, or per step within one run.
//
// The run id is a positional rather than a --run flag, because naming a run
// IS choosing the deeper view. As a flag it had to imply --cost to mean
// anything, which is a flag that reads as configured while binding nothing.
type RunsCostCmd struct {
	ReadFlags `embed:""`
	RunID     string `arg:""       help:"break this one run down per step" optional:""`
	Limit     int    `default:"20" help:"maximum number of rows to show"`
}

// Run prints per-run totals, or one run's steps.
func (r *RunsCostCmd) Run() error {
	if nothingRecorded(r.ReadFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	ctx := context.Background()

	if r.RunID != "" {
		return r.printRunCost(ctx, st)
	}

	return r.printCostTotals(ctx, st)
}

// RunsWhereCmd is which machines a run's placed steps ran on.
type RunsWhereCmd struct {
	ReadFlags `embed:""`
	RunID     string `arg:""                                 help:"the run to report on (default: the newest)" optional:""`
	Job       string `help:"take the newest run of this job"`
}

// Run prints one run's placements.
func (r *RunsWhereCmd) Run() error {
	if nothingRecorded(r.ReadFlags, noRunsYet(r.Pipeline)) {
		return nil
	}

	st, done, err := openRecorded(r.ReadFlags)
	if err != nil {
		return err
	}
	defer done()

	return r.printPlacements(context.Background(), st)
}

// nothingRecorded reports — and says — that a pipeline has no state file yet.
//
// Asked BEFORE opening, so asking about history never creates the database it
// is asking about: a read command that left a .steps/ behind would be a
// surprising thing for `steps runs` to do on a fresh checkout. It is a
// separate question from opening rather than a third return value, because a
// helper that answers "here is the store" and "there is no store" through one
// signature hands every caller a nil it must remember to check — which is
// exactly what a sixth `runs` subcommand written by copying the other five
// would forget.
func nothingRecorded(flags ReadFlags, answer string) bool {
	if !stateEmpty(flags) {
		return false
	}

	fmt.Println(answer)

	return true
}

func stateEmpty(flags ReadFlags) bool {
	path := flags.state()

	_, err := os.Stat(path)
	if err != nil {
		return true
	}

	// A file with no schema in it is the same answer as no file: a writer
	// creates the database before it fills it in, so a reader arriving in
	// that window must not report the operator's brand new database as one
	// written by a different version of steps.
	return sqlite.HasNothingRecorded(path)
}

// noRunsYet is the sentence every `steps runs` view says when the pipeline has
// no state file.
func noRunsYet(name string) string {
	if name == "" {
		return "no runs recorded yet"
	}

	return "no runs recorded yet for " + name
}

// openRecorded opens a pipeline's recorded state for reading. It returns a
// usable store or an error, never both nil — call nothingRecorded first.
//
// OpenExisting, not OpenStore: asking must not register the pipeline it is
// asking about — see sqlite.OpenExisting.
func openRecorded(flags ReadFlags) (store.Store, func(), error) {
	if flags.Pipeline == "" {
		return nil, nil, errors.New("which pipeline? pass -p <name> — `steps pipeline list` says what a daemon holds")
	}

	st, err := sqlite.OpenExisting(flags.state(), flags.Pipeline)
	if err != nil {
		return nil, nil, fmt.Errorf("could not open state store: %w", err)
	}

	return st, func() { _ = st.Close() }, nil
}

// runAcross reports on every pipeline in one state file: what it holds, and
// the newest runs across all of it.
//
// This is the CLI's answer to the question --db created. `steps runs` is
// otherwise scoped by the pipeline it is handed, so a file with three
// pipelines in it took three invocations to read and gave no interleaving at
// all — the web root has answered this since it learned to serve several
// pipelines, and it reads through the same store.Reader.
//
// The view is chosen by whether a pipeline was NAMED, not by how many the
// file turns out to hold: a one-pipeline file still prints the pipeline
// column here, so a script that reads this output gets the same columns
// whatever the file grows into.
func (r *RunsListCmd) runAcross() error {
	path := r.state()

	// The only flag left that a whole file cannot answer: RecentRuns spans
	// pipelines and does not filter by job, and two pipelines calling a job
	// `build` are not one job. Refused rather than silently ignored.
	if r.Job != "" {
		return fmt.Errorf("--job asks about one pipeline: run `steps runs list -p <name> --job %s --db %s`", r.Job, path)
	}

	// Stat first, for the same reason the scoped path does: asking about
	// history must not create the database it is asking about.
	_, err := os.Stat(path)
	if err != nil {
		fmt.Printf("no state database at %s\n", path)

		return nil
	}

	reader, err := sqlite.OpenReader(path)
	if errors.Is(err, store.ErrNoState) {
		// Created but not yet filled in — a writer is mid-first-open. Nothing
		// is recorded, which is an answer, not a file to delete.
		fmt.Printf("no pipelines recorded in %s\n", path)

		return nil
	}

	if err != nil {
		return fmt.Errorf("could not open state store: %w", err)
	}
	defer func() { _ = reader.Close() }()

	ctx := context.Background()

	pipelines, err := reader.Pipelines(ctx)
	if err != nil {
		return fmt.Errorf("could not read pipelines: %w", err)
	}

	if len(pipelines) == 0 {
		fmt.Printf("no pipelines recorded in %s\n", path)

		return nil
	}

	err = printPipelines(pipelines)
	if err != nil {
		return err
	}

	return r.printRunsAcross(ctx, reader, pipelines, path)
}

// printPipelines lists what the file holds. A name alone does not say which
// YAML is behind it — the name is the identity and the path is only what a
// human reads back — so both are printed.
func printPipelines(pipelines []store.PipelineRow) error {
	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "PIPELINE\tPATH")

	for _, pipeline := range pipelines {
		// Empty when no command that LOADED the YAML has opened this
		// pipeline yet, which a file written by an older build can hold. A
		// dash rather than a blank column, so the row reads as unanswered
		// rather than as a pipeline living at "".
		path := pipeline.Path
		if path == "" {
			path = "-"
		}

		_, _ = fmt.Fprintf(writer, "%s\t%s\n", pipeline.Name, path)
	}

	err := flush(writer)
	if err != nil {
		return err
	}

	fmt.Println()

	return nil
}

// printRunsAcross prints the interleaved feed, newest first.
//
// Every pipeline in the FILE is named, which is where this parts company with
// the web root: that one names only the pipelines the process serves, because
// a row it cannot link anywhere is worse than a missing one. The CLI serves
// nothing and links nowhere, so a run recorded by a pipeline whose YAML has
// since moved is still an answer to what ran.
//
// Runs rather than job_runs, which is what the scoped default reads: job_runs
// is the merkle cache index, keyed and upserted by content hash, so its
// timestamp means "last time this content ran" rather than "a build
// happened". Across pipelines the useful row is a real run with an id, which
// is the handle for going and asking that pipeline about it.
func (r *RunsListCmd) printRunsAcross(ctx context.Context, reader store.Reader, pipelines []store.PipelineRow, path string) error {
	names := make([]string, 0, len(pipelines))
	for _, pipeline := range pipelines {
		names = append(names, pipeline.Name)
	}

	rows, err := reader.RecentRuns(ctx, names, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read runs: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no runs recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tPIPELINE\tJOB\tSTATUS\tRUN\tCONFIG")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			formatWhen(row.StartedAt), row.Pipeline, row.JobName, row.Status, row.ID, shortConfig(row.ConfigSHA))
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	// The path rather than a Description: a Reader has no pipeline and no
	// handle, and this view only opens sqlite files (see runAcross).
	fmt.Printf("\nbreak one down with: steps runs cost -p <pipeline> <run> --db %s\n", path)

	return nil
}

// printJobRuns lists what actually ran, newest first.
//
// The runs table, not job_runs, which is what this view read until it was
// caught: job_runs is the merkle CACHE index, and recordChainSucceeded skips
// a chain containing a put or an agent because such a chain is never
// skippable — so the default history view of an agent pipeline recorded every
// failure and no success, and read as all-red or empty after a run that
// worked. It is also upserted by content hash, so twenty forced re-runs were
// one row.
//
// This is the same source the web UI and the cross-pipeline view read, which
// is the other half of the fix: one command that meant two different tables
// depending on whether a pipeline was named is a command nobody can reason
// about. The error text moves one command along, to `steps runs steps`, which
// reports it per step — where the answer to "why did it fail" actually is.
func (r *RunsListCmd) printJobRuns(ctx context.Context, st interface {
	store.Runs
	store.Meta
},
) error {
	rows, err := st.ListRuns(ctx, r.Job, r.Limit)
	if err != nil {
		return fmt.Errorf("could not read runs: %w", err)
	}

	if len(rows) == 0 {
		fmt.Println("no runs recorded")

		return nil
	}

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "WHEN\tJOB\tSTATUS\tRUN\tCONFIG")

	for _, row := range rows {
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			formatWhen(row.StartedAt), row.JobName, row.Status, row.ID, shortConfig(row.ConfigSHA))
	}

	err = flush(writer)
	if err != nil {
		return err
	}

	fmt.Printf("\nwhy a step did what it did: steps runs steps -p %s%s\n", r.Pipeline, dbNote(r.DB, st))

	return nil
}

func (r *RunsStepsCmd) printSteps(ctx context.Context, st store.Cache) error {
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

// printRunSteps lists what one run completed, build by build.
//
// No index column: a build's remainder counts from 0, so an index would read
// as a plan position it is not. Checked with FindRunRow first, which is
// scoped, so another pipeline's run — or a typo — is an error rather than an
// empty table that reads as "completed nothing".
func (r *RunsStepsCmd) printRunSteps(ctx context.Context, st interface {
	store.Meta
	store.Runs
}) error {
	run, ok, err := st.FindRunRow(ctx, r.RunID)
	if err != nil {
		return fmt.Errorf("could not read run %q: %w", r.RunID, err)
	}

	if !ok {
		return fmt.Errorf("no run %q was recorded for pipeline %q", r.RunID, st.Pipeline())
	}

	steps, err := st.CompletedRunSteps(ctx, r.RunID)
	if err != nil {
		return fmt.Errorf("could not read steps: %w", err)
	}

	if len(steps) == 0 {
		fmt.Printf("run %s completed no steps\n", run.ID)

		return nil
	}

	if r.Limit > 0 && len(steps) > r.Limit {
		steps = steps[:r.Limit]
	}

	fmt.Printf("run %s  %s  %s: steps a --resume skips\n", run.ID, run.JobName, run.Status)

	writer := newTabWriter()
	_, _ = fmt.Fprintln(writer, "BUILD\tSTEP")

	for _, step := range steps {
		build := strings.TrimPrefix(step.BuildID, run.ID)
		if build == "" {
			build = "-"
		}

		_, _ = fmt.Fprintf(writer, "%s\t%s\n", build, step.Name)
	}

	return flush(writer)
}

func (r *RunsQueueCmd) printQueue(ctx context.Context, st store.Queue) error {
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

// printCostTotals lists what each recorded run's agent steps spent.
//
// The cache column is the one worth having: it is the only place prompt
// caching reports whether it did anything, and a run that suddenly drops from
// 60% to 0% is the visible half of a bill that doubled.
func (r *RunsCostCmd) printCostTotals(ctx context.Context, st interface {
	store.Usage
	store.Meta
},
) error {
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

	fmt.Printf("\nbreak one down with: steps runs cost -p %s <run>%s\n", r.Pipeline, dbNote(r.DB, st))

	return nil
}

// dbNote carries --db into a printed follow-up command.
//
// Without it the hint names a DIFFERENT database than the one it was just
// printed from: the default path is derived from the pipeline, so a reader who
// copies the line after `steps runs cost app.yml --db shared.db` is sent to
// `.steps/app.yml.db` and told there is nothing there.
//
// The store's Description rather than the flag as typed: it is the form the
// driver calls safe to print, which for a network database means without its
// credentials.
func dbNote(typed DB, st store.Meta) string {
	if typed == "" {
		return ""
	}

	return " --db " + st.Description()
}

// printRunCost breaks one run down per agent step.
func (r *RunsCostCmd) printRunCost(ctx context.Context, st store.Usage) error {
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

// placementReader is `steps runs where`: the placements themselves, plus the
// run history it resolves "the last run of this job" through when no run id
// was given.
type placementReader interface {
	store.Placements
	store.Runs
}

// printPlacements says which machines a run's placed steps ran on, and what
// those machines turned out to be.
//
// The answer to "it passes locally and fails on the fleet". Facts and never a
// price: what an instance-hour cost is not knowable from here — list prices
// ignore Savings Plans, a spot instance's paid price is reported by no API,
// and real billing lands a day later — and a confident wrong number in a cost
// column is worse than no column. Anyone holding their own rate card can
// price these rows.
//
// Rendered through web.PlacementView, the same type the run page draws: one
// spelling of what a machine was, so the browser and the terminal cannot
// disagree about it. They did — the terminal's copy never learned which
// filesystems are memory.
func (r *RunsWhereCmd) printPlacements(ctx context.Context, st placementReader) error {
	run, ok, err := r.placementRun(ctx, st)
	if err != nil || !ok {
		return err
	}

	placements, err := st.RunPlacements(ctx, run.ID)
	if err != nil {
		return fmt.Errorf("could not read placements: %w", err)
	}

	if len(placements) == 0 {
		// Distinguished from "this run had no placed steps", which is the
		// ordinary case for a pipeline that names no worker. Past tense only
		// about a run that is over: a placement is recorded when the step
		// finishes, so a run still in flight has nothing recorded YET.
		if run.Status == "running" {
			fmt.Printf("no placed steps recorded for run %s yet\n", run.ID)
		} else {
			fmt.Printf("run %s ran every step on this machine\n", run.ID)
		}

		return nil
	}

	fmt.Printf("%-24s  %-12s  %-13s  %-28s  %9s  %9s  %s\n",
		"STEP", "TAG", "PLATFORM", "FILESYSTEM", "SENT", "RECEIVED", "MACHINE")

	memory := false

	for _, placed := range placements {
		view := web.PlacementView{Placement: placed}

		filesystem := view.Filesystem()
		if view.Volatile() {
			filesystem += " [RAM]"
			memory = true
		}

		fmt.Printf("%-24s  %-12s  %-13s  %-28s  %9s  %9s  %s\n",
			truncateName(placed.StepName, 24), truncateName(placed.Tag, 12),
			view.Platform(), filesystem, view.Sent(), view.Received(), view.Machine())
	}

	if memory {
		fmt.Println("\n[RAM] that workdir is memory, not disk: the pushed binary and the step's tree spend it, and a reboot loses both. Name a path on a real disk in the worker URL.")
	}

	return nil
}

// placementRun is the run --where reports on: the one named, or the newest.
//
// A named run is looked up rather than taken on trust. RunPlacements is
// pipeline-scoped, so a typo — or a run belonging to another pipeline sharing
// this state file — reads back as zero rows, and the caller would print that
// as a run that ran every step here: a positive claim about a run this
// pipeline has never seen.
func (r *RunsWhereCmd) placementRun(ctx context.Context, st placementReader) (store.RunRow, bool, error) {
	if r.RunID != "" {
		run, ok, err := st.FindRunRow(ctx, r.RunID)
		if err != nil {
			return store.RunRow{}, false, fmt.Errorf("could not read run: %w", err)
		}

		if !ok {
			fmt.Printf("no run %s in this pipeline\n", r.RunID)
		}

		return run, ok, nil
	}

	runs, err := st.ListRuns(ctx, r.Job, 1)
	if err != nil {
		return store.RunRow{}, false, fmt.Errorf("could not read runs: %w", err)
	}

	if len(runs) == 0 {
		fmt.Println("no runs recorded")

		return store.RunRow{}, false, nil
	}

	return runs[0], true, nil
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
// Never $0.00 for an unreported cost: only a CLI-backed agent reports dollars
// at all, so a zero here would say every hosted run was free rather than that
// nobody priced it. See the agent_usage schema comment.
//
// web.FormatUSD rather than a local %.2f, and for the same reason the absent
// case is not a zero: a CLI step routinely costs fractions of a cent, and two
// decimals round exactly those runs to the "$0.00" this function exists to
// never print.
func renderCost(cost *float64, unpriced int) string {
	if cost == nil {
		return "unpriced"
	}

	rendered := web.FormatUSD(*cost)
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
