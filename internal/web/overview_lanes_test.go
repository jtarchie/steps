package web

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// lanePipelines is testPipelines with each pipeline's YAML given, for the overview tests that turn on what a pipeline declares: its job order, its passed: edges, its triggers. Pairs are name, then YAML.
func lanePipelines(t *testing.T, opts []Option, pairs ...string) (*Server, []*Pipeline) {
	t.Helper()

	dir := t.TempDir()
	statePath := filepath.Join(dir, ".steps", "shared.db")

	var pipelines []*Pipeline

	for i := 0; i+1 < len(pairs); i += 2 {
		name, body := pairs[i], pairs[i+1]
		path := filepath.Join(dir, name+".yml")
		writeFile(t, path, body)

		cfg, err := config.LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", name, err)
		}

		st, err := sqlite.OpenStore(statePath, name)
		if err != nil {
			t.Fatalf("OpenStore(%s): %v", name, err)
		}

		t.Cleanup(func() { _ = st.Close() })

		pipelines = append(pipelines, NewPipeline(name, path, cfg, st, events.New(nil)))
	}

	server, err := New(pipelines, nil, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return server, pipelines
}

const twoJobs = `
resources:
  - name: repo
    type: git
    source: {uri: /tmp/nowhere}
jobs:
  - name: build
    plan:
      - get: repo
        trigger: true
      - task: work
        run: "true"
  - name: deploy
    plan:
      - get: repo
        passed: [build]
      - task: work
        run: "true"
`

const weeklyCron = `
resources:
  - name: weekly
    type: cron
    source: {expression: "0 7 * * 0", location: America/Denver}
jobs:
  - name: build
    plan:
      - get: weekly
        trigger: true
      - task: work
        run: "true"
`

// handedOver triggers deploy on what build passed, the way knowhere-ship's deploy does: the pipeline waits on no outside resource for it.
const handedOver = `
resources:
  - name: weekly
    type: cron
    source: {expression: "0 7 * * 0"}
  - name: db
    type: git
    source: {uri: /tmp/nowhere}
jobs:
  - name: build
    plan:
      - get: weekly
        trigger: true
      - get: db
  - name: deploy
    plan:
      - get: db
        trigger: true
        passed: [build]
      - task: work
        run: "true"
`

const threeTriggers = `
resources:
  - name: alpha
    type: git
    source: {uri: /tmp/nowhere}
  - name: beta
    type: git
    source: {uri: /tmp/nowhere}
  - name: gamma
    type: git
    source: {uri: /tmp/nowhere}
jobs:
  - name: answer
    plan:
      - get: alpha
        trigger: true
      - get: beta
        trigger: true
      - get: gamma
        trigger: true
`

const oneManualJob = `
jobs:
  - name: audit
    plan:
      - task: work
        run: "true"
`

// lastRunCell is the overview row's Latest cell.
func lastRunCell(t *testing.T, page, slug string) string {
	t.Helper()

	return between(t, pipelineRow(t, page, slug), `<td class="lastrun">`, "</td>")
}

// TestEveryJobIsNamedOnItsCell: a cell named its job only on hover, and the red ones were named again beside the strip, so a row's text moved with its health. Every cell carries its name; nothing is named twice.
func TestEveryJobIsNamedOnItsCell(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "failed")

	_, page := get(t, server, "/")
	row := pipelineRow(t, page, "app")

	for _, want := range []string{
		`<a class="cell st-failed" href="/p/app/jobs/build" title="build: failed" aria-label="build, failed" tabindex="-1"><span class="glyph" aria-hidden="true">✗</span><span aria-hidden="true">build</span></a>`,
		`<a class="cell st-none" href="/p/app/jobs/deploy" title="deploy: never ran" aria-label="deploy, never ran" tabindex="-1"><span class="glyph" aria-hidden="true"></span><span aria-hidden="true">deploy</span></a>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the row lacks a named cell %s:\n%s", want, row)
		}
	}

	if strings.Contains(row, `class="named`) {
		t.Errorf("a red job is still named a second time beside the strip:\n%s", row)
	}
}

// TestOnlyARedCellKeepsItsGlyph: a green row said ✓ once per job on top of its mark. The mark speaks for the row; a cell draws a glyph only when it is the reason the row is red, since red-against-green tint is the one difference red-green colour blindness cannot see.
func TestOnlyARedCellKeepsItsGlyph(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "succeeded")
	seedRun(t, pipelines[0], "run-2", "deploy", "failed")

	_, page := get(t, server, "/")
	row := pipelineRow(t, page, "app")

	for _, want := range []string{
		`<a class="cell st-passed" href="/p/app/jobs/build" title="build: passed" aria-label="build, passed" tabindex="-1"><span class="glyph" aria-hidden="true"></span>`,
		`<a class="cell st-failed" href="/p/app/jobs/deploy" title="deploy: failed" aria-label="deploy, failed" tabindex="-1"><span class="glyph" aria-hidden="true">✗</span>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the row lacks %s:\n%s", want, row)
		}
	}
}

// TestAJobFedByAnotherIsDrawnAfterAnArrow: an arrow between unrelated jobs would claim a passed: edge the pipeline does not have.
func TestAJobFedByAnotherIsDrawnAfterAnArrow(t *testing.T) {
	t.Parallel()

	independent := `
jobs:
  - name: snapshot
    plan: [{task: work, run: "true"}]
  - name: release
    plan: [{task: work, run: "true"}]
`
	server, _ := lanePipelines(t, nil, "app", twoJobs, "rel", independent)

	_, page := get(t, server, "/")

	if row := pipelineRow(t, page, "app"); !strings.Contains(row, `<span class="jobsep" aria-hidden="true">→</span>`) {
		t.Errorf("deploy gets repo passed: [build] but is not drawn after an arrow:\n%s", row)
	}

	if row := pipelineRow(t, page, "rel"); strings.Contains(row, "→") || !strings.Contains(row, `<span class="jobsep" aria-hidden="true">·</span>`) {
		t.Errorf("two unrelated jobs are joined by an arrow, or not separated:\n%s", row)
	}
}

// TestLatestFollowsTheRedJob: Latest was the pipeline's newest run of ANY job, so a row with a failed release and a later passing snapshot was red in its mark and its cell and green in words. On a red row it names the run that made it red.
func TestLatestFollowsTheRedJob(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-red", "build", "failed")
	seedRun(t, pipelines[0], "run-green", "deploy", "succeeded")

	_, page := get(t, server, "/")
	cell := lastRunCell(t, page, "app")

	if !strings.Contains(cell, `href="/p/app/runs/run-red"`) || !strings.Contains(cell, ">failed<") || !strings.Contains(cell, "build") {
		t.Errorf("a red row's Latest does not name the failed build run:\n%s", cell)
	}

	if strings.Contains(cell, "run-green") || strings.Contains(cell, "passed") {
		t.Errorf("a red row's Latest still reports the newest run of another job:\n%s", cell)
	}
}

// TestLatestFollowsAnErroredJobToo: an errored job's mark is not the failed disc the row's stripe keys on, and a gate on the stripe let a later green run of another job stand as Latest beside it.
func TestLatestFollowsAnErroredJobToo(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-err", "build", "errored")
	seedRun(t, pipelines[0], "run-green", "deploy", "succeeded")

	_, page := get(t, server, "/")

	if cell := lastRunCell(t, page, "app"); !strings.Contains(cell, `href="/p/app/runs/run-err"`) {
		t.Errorf("an errored job's row reports a later green run as Latest:\n%s", cell)
	}
}

// TestAGreenRowsLatestNamesItsJob: off a red row, Latest is still the newest run, named by job, and says only which and when: the row mark and every cell already say passed, so a third ✓ made the healthy column the loudest one.
func TestAGreenRowsLatestNamesItsJob(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "build", "succeeded")
	seedRun(t, pipelines[0], "run-2", "deploy", "succeeded")

	_, page := get(t, server, "/")
	cell := lastRunCell(t, page, "app")

	if !strings.Contains(cell, `href="/p/app/runs/run-2"`) || !strings.Contains(cell, "deploy") {
		t.Errorf("a green row's Latest is not its newest run, named by job:\n%s", cell)
	}

	if strings.Contains(cell, "st-passed") || strings.Contains(cell, "✓") {
		t.Errorf("a green row's Latest repeats passed in color or glyph:\n%s", cell)
	}

	if strings.Count(cell, "passed") != 1 || !strings.Contains(cell, `<span class="visually-hidden">passed </span>`) {
		t.Errorf("a green row's Latest shows passed, or stops telling a screen reader:\n%s", cell)
	}
}

// TestARedRowsLatestStillSaysWhat: quieting the green row must not quiet the red one, but the word is enough: the mark and the red cell already drew ✗.
func TestARedRowsLatestStillSaysWhat(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", oneManualJob, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "audit", "failed")

	_, page := get(t, server, "/")

	cell := lastRunCell(t, page, "app")
	if !strings.Contains(cell, `<a class="st-failed" href="/p/app/runs/run-1">failed</a>`) {
		t.Errorf("a red row's Latest lost its red word:\n%s", cell)
	}

	if strings.Contains(cell, `class="st `) {
		t.Errorf("a red row's Latest draws a third glyph beside the mark's and the cell's:\n%s", cell)
	}
}

// TestARunningLatestSaysHowLongItHasRun: "running 7m 55s ago" was the start time phrased as an age. A run in flight counts up from its start; a finished one is one coarse unit, because "23h 17m ago" is precision nobody acts on.
func TestARunningLatestSaysHowLongItHasRun(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "busy", oneManualJob, "done", oneManualJob)
	seedRun(t, pipelines[0], "run-live", "audit", "")
	seedRun(t, pipelines[1], "run-over", "audit", "succeeded")

	_, page := get(t, server, "/")

	busy := lastRunCell(t, page, "busy")
	if !strings.Contains(busy, "data-elapsed-since=") || strings.Contains(busy, "ago") {
		t.Errorf("a running Latest is not an elapsed count:\n%s", busy)
	}

	done := lastRunCell(t, page, "done")
	if strings.Contains(done, "data-ago=") || !strings.Contains(done, `<time datetime=`) {
		t.Errorf("a finished Latest is not the one-unit compact age:\n%s", done)
	}
}

// TestEachJobCarriesItsRecentRuns: the strip under a job is its last ten runs oldest first, so "broke once after nine greens" and "red three times running" stop looking alike. Errored keeps its own class: a step said no is a different fix from the machine said no.
func TestEachJobCarriesItsRecentRuns(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", oneManualJob, "other", oneManualJob)

	for i := range 9 {
		seedRun(t, pipelines[0], fmt.Sprintf("old-%d", i), "audit", "succeeded")
	}

	seedRun(t, pipelines[0], "r-fail", "audit", "failed")
	seedRun(t, pipelines[0], "r-err", "audit", "errored")
	seedRun(t, pipelines[0], "r-live", "audit", "")

	_, page := get(t, server, "/")
	hist := between(t, pipelineRow(t, page, "app"), `<span class="hist" aria-hidden="true">`, "</span>")

	if got := strings.Count(hist, `<i class="tick`); got != 10 {
		t.Errorf("the strip holds %d runs, want the last 10:\n%s", got, hist)
	}

	if !strings.HasSuffix(hist, `<i class="tick st-failed"></i><i class="tick st-errored"></i><i class="tick st-running"></i>`) {
		t.Errorf("the strip does not end on failed, errored, running, oldest first:\n%s", hist)
	}

	if strings.Contains(pipelineRow(t, page, "other"), `class="hist"`) {
		t.Error("a job that never ran draws an empty strip")
	}
}

// TestEveryStripHoldsTenSlots: a strip as long as a job's runs read as a progress bar, and a new job's single tick as a stray dot. Ten slots, newest at the right, unfilled ones faint, so a short strip says new rather than broken.
func TestEveryStripHoldsTenSlots(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", oneManualJob, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-1", "audit", "succeeded")

	_, page := get(t, server, "/")
	hist := between(t, pipelineRow(t, page, "app"), `<span class="hist" aria-hidden="true">`, "</span>")

	if got := strings.Count(hist, `<i class="tick`); got != laneRuns {
		t.Errorf("a one-run strip holds %d slots, want %d:\n%s", got, laneRuns, hist)
	}

	if got := strings.Count(hist, `<i class="tick empty"></i>`); got != laneRuns-1 {
		t.Errorf("a one-run strip has %d empty slots, want %d:\n%s", got, laneRuns-1, hist)
	}

	if !strings.HasSuffix(hist, `<i class="tick st-passed"></i>`) {
		t.Errorf("the one run is not the newest slot, at the right:\n%s", hist)
	}
}

// TestAPipelineWithNoJobsSaysSo: a blank Jobs cell reads as a page that failed to draw.
func TestAPipelineWithNoJobsSaysSo(t *testing.T) {
	t.Parallel()

	server, _ := lanePipelines(t, nil, "ghost", "jobs: []\n", "other", oneManualJob)

	_, page := get(t, server, "/")

	if cell := between(t, pipelineRow(t, page, "ghost"), `<td class="jobchips">`, "</td>"); !strings.Contains(cell, "no jobs") {
		t.Errorf("an empty pipeline's Jobs cell does not say so:\n%s", cell)
	}

	if cell := between(t, pipelineRow(t, page, "other"), `<td class="jobchips">`, "</td>"); strings.Contains(cell, "no jobs") {
		t.Errorf("a pipeline with a job says it has none:\n%s", cell)
	}
}

// TestNextNamesOneTriggerAndCountsTheRest: three trigger names pushed the table sideways. The first is named, the rest counted, and the whole list is still there for a pointer and a screen reader.
func TestNextNamesOneTriggerAndCountsTheRest(t *testing.T) {
	t.Parallel()

	server, _ := lanePipelines(t, nil, "bots", threeTriggers, "other", oneManualJob)

	_, page := get(t, server, "/")
	cell := between(t, pipelineRow(t, page, "bots"), `<td class="next">`, "</td>")

	if !strings.Contains(cell, `title="on alpha, beta, gamma"`) {
		t.Errorf("Next does not keep the whole list in its title:\n%s", cell)
	}

	if !strings.Contains(cell, `on alpha<span aria-hidden="true"> +2</span><span class="visually-hidden">, beta, gamma</span>`) {
		t.Errorf("Next does not name the first trigger and count the rest:\n%s", cell)
	}
}

// TestNextNamesALoneTriggerWhole: a count of zero is noise.
func TestNextNamesALoneTriggerWhole(t *testing.T) {
	t.Parallel()

	server, _ := lanePipelines(t, nil, "git", twoJobs, "other", oneManualJob)

	_, page := get(t, server, "/")

	if cell := between(t, pipelineRow(t, page, "git"), `<td class="next">`, "</td>"); !strings.Contains(cell, "on repo") || strings.Contains(cell, "+") {
		t.Errorf("a single trigger is not named alone:\n%s", cell)
	}
}

// TestAWidePipelineStripsOnlyItsRedJobs: past four jobs a strip on every one wraps the row, so only the jobs that want a person keep theirs.
func TestAWidePipelineStripsOnlyItsRedJobs(t *testing.T) {
	t.Parallel()

	var wide strings.Builder

	wide.WriteString("jobs:\n")

	for _, name := range []string{"a", "b", "c", "d", "e"} {
		wide.WriteString("  - name: " + name + "\n    plan: [{task: work, run: \"true\"}]\n")
	}

	server, pipelines := lanePipelines(t, nil, "wide", wide.String(), "other", oneManualJob)

	for _, name := range []string{"a", "b", "c", "d"} {
		seedRun(t, pipelines[0], "run-"+name, name, "succeeded")
	}

	seedRun(t, pipelines[0], "run-e", "e", "failed")

	_, page := get(t, server, "/")

	if got := strings.Count(pipelineRow(t, page, "wide"), `class="hist"`); got != 1 {
		t.Errorf("a five-job pipeline draws %d strips, want one, on its red job", got)
	}
}

// TestTheOverviewSaysWhatHappensNext: whether a red pipeline retries on its own or waits for a person is the question after "is it red". A cron says when, a trigger says on what, nothing says manual, and a paused pipeline says that instead of a time that will not come.
func TestTheOverviewSaysWhatHappensNext(t *testing.T) {
	t.Parallel()

	fire := time.Now().Add(5*time.Hour + 30*time.Minute).Truncate(time.Second)

	var asked []string

	next := WithCronNext(askedAbout(&asked, fire))

	server, _ := lanePipelines(t, []Option{next},
		"cron", weeklyCron, "git", twoJobs, "manual", oneManualJob)

	code, page := get(t, server, "/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}

	if len(asked) == 0 || asked[0] != "weekly" {
		t.Errorf("the cron reader was asked about %v, want the weekly resource", asked)
	}

	nextCell := func(slug string) string {
		return between(t, pipelineRow(t, page, slug), `<td class="next">`, "</td>")
	}

	if cell := nextCell("cron"); !strings.Contains(cell, fmt.Sprintf(`datetime=%q`, fire.UTC().Format(time.RFC3339))) || !strings.Contains(cell, ">in 5h<") || !strings.Contains(cell, fire.Format("Mon Jan 2 15:04 MST")) {
		t.Errorf("a cron pipeline does not say how long until it fires, with the clock time beside:\n%s", cell)
	}

	if cell := nextCell("git"); !strings.Contains(cell, "on repo") {
		t.Errorf("a triggered pipeline does not name what it waits on:\n%s", cell)
	}

	if cell := nextCell("manual"); !strings.Contains(cell, "manual") {
		t.Errorf("a pipeline nothing triggers does not say manual:\n%s", cell)
	}
}

// TestAHandOffIsNotAnOutsideTrigger: a trigger with passed: fires because an upstream job finished, so naming it beside the cron's time says the pipeline waits on something it does not.
func TestAHandOffIsNotAnOutsideTrigger(t *testing.T) {
	t.Parallel()

	var asked []string

	server, _ := lanePipelines(t, []Option{WithCronNext(askedAbout(&asked, time.Now().Add(time.Hour)))},
		"ship", handedOver, "other", oneManualJob)

	_, page := get(t, server, "/")

	if cell := between(t, pipelineRow(t, page, "ship"), `<td class="next">`, "</td>"); strings.Contains(cell, " on ") || !strings.Contains(cell, "<time") {
		t.Errorf("a passed: hand-off is listed as an outside trigger:\n%s", cell)
	}
}

// TestAPausedPipelinePromisesNoTime: its cron slot passes with nothing minted, so a time there would be a lie; and the row mark already says paused, so the cell says nothing.
func TestAPausedPipelinePromisesNoTime(t *testing.T) {
	t.Parallel()

	var asked []string

	server, pipelines := lanePipelines(t, []Option{WithCronNext(askedAbout(&asked, time.Now().Add(time.Hour)))},
		"held", weeklyCron, "other", oneManualJob)

	err := pipelines[0].Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	_, page := get(t, server, "/")

	if cell := between(t, pipelineRow(t, page, "held"), `<td class="next">`, "</td>"); cell != `<td class="next">` {
		t.Errorf("a paused cron pipeline promises a time:\n%s", cell)
	}
}

// askedAbout is a CronNext that answers fire for every resource and records which it was asked about.
func askedAbout(asked *[]string, fire time.Time) CronNext {
	return func(res config.Resource, _ time.Time) (time.Time, error) {
		*asked = append(*asked, res.Name)

		return fire, nil
	}
}

// TestUntilCompactCountsDown: a clock time named the wrong day a week out, and needed a today/tomorrow to read at all. How long until, in one coarse unit, is right at any distance; the clock time is in the title.
func TestUntilCompactCountsDown(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in          time.Duration
		short, said string
	}{
		{5*time.Hour + 47*time.Minute, "in 5h", "in 5 hours"},
		{12 * time.Minute, "in 12m", "in 12 minutes"},
		{7*24*time.Hour - 2*time.Hour, "in 6d", "in 6 days"},
		{time.Hour, "in 1h", "in 1 hour"},
		{30 * time.Second, "in 30s", "in 30 seconds"},
		{-time.Second, "now", "now"},
	} {
		if short, said := untilCompact(tc.in); short != tc.short || said != tc.said {
			t.Errorf("untilCompact(%s) = %q, %q; want %q, %q", tc.in, short, said, tc.short, tc.said)
		}
	}
}

// TestLatestIsTheNewestRedRun: with two red jobs, the first in config order was Latest however old its failure, so a fresh break read as days old.
func TestLatestIsTheNewestRedRun(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-old", "build", "failed")
	seedRun(t, pipelines[0], "run-new", "deploy", "failed")

	_, page := get(t, server, "/")

	if cell := lastRunCell(t, page, "app"); !strings.Contains(cell, `href="/p/app/runs/run-new"`) {
		t.Errorf("a row with two red jobs does not report the newer failure as Latest:\n%s", cell)
	}
}

// TestAHeldJobPromisesNoCronSlot: the breaker drops a held job's automatic triggers at dispatch, so its cron slot is a time nothing will happen at.
func TestAHeldJobPromisesNoCronSlot(t *testing.T) {
	t.Parallel()

	var asked []string

	server, pipelines := lanePipelines(t, []Option{WithCronNext(askedAbout(&asked, time.Now().Add(time.Hour)))},
		"cron", weeklyCron, "other", oneManualJob)

	held, _, err := pipelines[0].Store.RecordJobOutcome(t.Context(), "build", false, 1)
	if err != nil || !held {
		t.Fatalf("RecordJobOutcome = %v, %v: the job was not held", held, err)
	}

	_, page := get(t, server, "/")

	if cell := between(t, pipelineRow(t, page, "cron"), `<td class="next">`, "</td>"); strings.Contains(cell, "<time") || !strings.Contains(cell, "manual") {
		t.Errorf("a pipeline whose only cron job is held promises a slot:\n%s", cell)
	}
}

// TestACronFeedingTwoJobsIsReadOnce: Next is drawn every poll, and a cron named by N jobs was compiled N times to answer one question.
func TestACronFeedingTwoJobsIsReadOnce(t *testing.T) {
	t.Parallel()

	var asked []string

	server, _ := lanePipelines(t, []Option{WithCronNext(askedAbout(&asked, time.Now().Add(time.Hour)))}, "cron", `
resources:
  - name: weekly
    type: cron
    source: {expression: "0 7 * * 0"}
jobs:
  - name: a
    plan:
      - get: weekly
        trigger: true
  - name: b
    plan:
      - get: weekly
        trigger: true
`, "other", oneManualJob)

	get(t, server, "/")

	if len(asked) != 1 {
		t.Errorf("the cron reader was asked %d times for one resource: %v", len(asked), asked)
	}
}

// TestAnErroredRowIsRed: the header counted an errored pipeline as failing while its row had no stripe and the live region never announced it, so the one count that said "look" pointed at a row that did not.
func TestAnErroredRowIsRed(t *testing.T) {
	t.Parallel()

	server, pipelines := lanePipelines(t, nil, "app", oneManualJob, "other", oneManualJob)
	seedRun(t, pipelines[0], "run-err", "audit", "errored")

	_, page := get(t, server, "/")

	if row := pipelineRow(t, page, "app"); !strings.Contains(row, `<tr class="red" data-slug="app">`) {
		t.Errorf("an errored pipeline is not a red row:\n%s", row)
	}

	if news := between(t, page, `id="red-news"`, `</div>`); !strings.Contains(news, `<p id="red-app">app is failing: audit errored</p>`) {
		t.Errorf("an errored pipeline has no line in the live region:\n%s", news)
	}
}

// TestAScrollingTableHoldsItsHiddenWords: a visually-hidden span is absolutely positioned, so inside a table that scrolls sideways it escaped the scroller to the nearest positioned ancestor and widened the whole page — measured at 768px, a page 830px wide in a 753px window.
func TestAScrollingTableHoldsItsHiddenWords(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	_, css := get(t, server, "/static/app.css")
	if !cssRule(css, ".tablewrap", "position: relative") {
		t.Error("a .tablewrap is not the containing block of the hidden words inside it")
	}
}
