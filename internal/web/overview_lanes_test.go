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

// TestAGreenRowsLatestNamesItsJob: off a red row, Latest is still the newest run, now with the job it belongs to.
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

	fire := time.Date(2030, 1, 6, 7, 0, 0, 0, time.UTC)

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

	if cell := nextCell("cron"); !strings.Contains(cell, `datetime="2030-01-06T07:00:00Z"`) || !strings.Contains(cell, fire.Format(nextLayout(time.Now(), fire))) {
		t.Errorf("a cron pipeline does not say when it fires next:\n%s", cell)
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

// TestAPausedPipelinePromisesNoTime: its cron slot passes with nothing minted, so a time there would be a lie.
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

	if cell := between(t, pipelineRow(t, page, "held"), `<td class="next">`, "</td>"); !strings.Contains(cell, "paused") || strings.Contains(cell, "<time") {
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

// TestNextLayoutCoarsensWithDistance: a clock time a week out names the wrong day, and a weekday further out names the wrong week.
func TestNextLayoutCoarsensWithDistance(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(3 * time.Hour), "12:00"},
		{now.Add(48 * time.Hour), "Sun 09:00"},
		{now.Add(10 * 24 * time.Hour), "Nov 23"},
		// A weekly cron on its own day, after firing: next week's slot named by weekday reads as this morning.
		{now.Add(7*24*time.Hour - 2*time.Hour), "Nov 20"},
	} {
		if got := tc.at.Format(nextLayout(now, tc.at)); got != tc.want {
			t.Errorf("a fire at %s reads %q, want %q", tc.at, got, tc.want)
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
