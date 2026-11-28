package e2e

import (
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// routePipeline loops once: gate fails on the first lap, routes back to editor, and passes on the second.
func routePipeline(t *testing.T, path, laps string) {
	t.Helper()

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: editor
    inputs: []
    run: echo lap >> `+laps+`
  - task: gate
    inputs: []
    to: {failure: editor}
    max_visits: 3
    run: test "$(wc -l < `+laps+`)" -ge 2
`)
}

// transitionsPipeline holds every way a plan leaves straight-line order without an LLM: a retried step, a to: loop, and a tolerated failure.
func transitionsPipeline(t *testing.T, path, dir string) {
	t.Helper()

	tries, laps := filepath.Join(dir, "tries"), filepath.Join(dir, "laps")

	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - task: transcribe
    inputs: []
    attempts: 3
    run: |
      echo try >> `+tries+`
      echo "try $(wc -l < `+tries+` | tr -d ' ')"
      test "$(wc -l < `+tries+`)" -ge 3
  - task: editor
    inputs: []
    run: echo lap >> `+laps+`
  - task: gate
    inputs: []
    to: {failure: editor}
    max_visits: 3
    run: test "$(wc -l < `+laps+`)" -ge 2
  - try:
      task: notify
      inputs: []
      run: "false"
`)
}

// stepRows is every row on a run page drawn for a step named name, in page order, each from its opening div to the next row's.
func stepRows(page, name string) []string {
	starts := regexp.MustCompile(`<div id="step-\d+-`+regexp.QuoteMeta(name)+`"`).FindAllStringIndex(page, -1)
	rows := make([]string, 0, len(starts))

	for _, at := range starts {
		rest := page[at[0]:]

		end := len(rest) - 1
		if next := rowStart.FindStringIndex(rest[1:]); next != nil {
			end = next[0]
		}

		rows = append(rows, rest[:end+1])
	}

	return rows
}

var rowStart = regexp.MustCompile(`<div id="(step|loop|pass)-`)

var tabPattern = regexp.MustCompile(`<button[^>]*role="tab"[^>]*>[^<]*</button>`)

// tab is one tab of a strip as a reader meets it: what a screen reader says, whether it is the one shown, and the glyph and number drawn.
type tab struct {
	label, shown string
	selected     bool
}

// tabsIn reads the first tab strip in markup.
func tabsIn(t *testing.T, markup string) []tab {
	t.Helper()

	start := strings.Index(markup, `role="tablist"`)
	if start < 0 {
		t.Fatalf("no tab strip in:\n%s", markup)
	}

	strip, _, _ := strings.Cut(markup[start:], `</span>`)
	buttons := tabPattern.FindAllString(strip, -1)
	tabs := make([]tab, 0, len(buttons))

	for _, button := range buttons {
		tabs = append(tabs, tab{
			label:    regexp.MustCompile(`aria-label="([^"]*)"`).FindStringSubmatch(button)[1],
			shown:    visibleText(button),
			selected: strings.Contains(button, `aria-selected="true"`),
		})
	}

	return tabs
}

// runPage runs the pipeline at path and returns its one run's page.
func runPage(t *testing.T, path string) string {
	t.Helper()

	mustRun(t, "run", path, "--job", "build")

	server, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}

	code, page := webGet(t, server, "/p/"+target.Slug+"/runs/"+runs[0].ID)
	if code != http.StatusOK {
		t.Fatalf("run page = %d", code)
	}

	return page
}

// TestARunPageDrawsALoopAsTabsOfItsPasses: a to: loop is one block over the steps it repeated, with a tab per pass the way Concourse tabs a retried step, each tab saying in words how its pass ended. It used to be the same rows again, with the decision in a banner above the transcript.
func TestARunPageDrawsALoopAsTabsOfItsPasses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	transitionsPipeline(t, path, dir)
	page := runPage(t, path)

	start := strings.Index(page, `<div id="loop-`)
	if start < 0 || !regexp.MustCompile(`^<div id="loop-\d+" class="step passed loop container"`).MatchString(page[start:]) {
		t.Fatalf("the loop is not drawn as one block:\n%s", page)
	}

	loop := page[start:strings.Index(page, `-notify"`)]

	tabs := tabsIn(t, loop)
	want := []tab{
		{label: "pass 1 of 2, routed to editor", shown: "↪1"},
		{label: "pass 2 of 2, passed", shown: "✓2", selected: true},
	}

	if len(tabs) != len(want) {
		t.Fatalf("loop tabs = %+v, want %+v", tabs, want)
	}

	for i := range want {
		if tabs[i] != want[i] {
			t.Errorf("loop tab %d = %+v, want %+v", i+1, tabs[i], want[i])
		}
	}

	if panels := strings.Count(loop, `role="tabpanel"`); panels != 2 {
		t.Errorf("the loop holds %d pass panels, want 2", panels)
	}

	if gates := stepRows(loop, "gate"); len(gates) != 2 {
		t.Errorf("the loop holds %d gate rows, want both passes'", len(gates))
	} else {
		assertRoutedRow(t, page, gates[0])
	}

	if strings.Contains(page, `class="note pass"`) {
		t.Error("a pass is still labelled in text beside a repeated row")
	}
}

// TestARunPageTabsAStepsAttempts: a step that retried says so on its folded row, with a tab per attempt, and each attempt's output sits under its own tab rather than stacked under loose "attempt n/m" lines.
func TestARunPageTabsAStepsAttempts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	transitionsPipeline(t, path, dir)
	page := runPage(t, path)

	rows := stepRows(page, "transcribe")
	if len(rows) != 1 {
		t.Fatalf("transcribe drew %d rows, want one row holding its attempts", len(rows))
	}

	tabs := tabsIn(t, rows[0])
	want := []tab{
		{label: "attempt 1 of 3, failed", shown: "✗1"},
		{label: "attempt 2 of 3, failed", shown: "✗2"},
		{label: "attempt 3 of 3, passed", shown: "✓3", selected: true},
	}

	if len(tabs) != len(want) {
		t.Fatalf("attempt tabs = %+v, want %+v", tabs, want)
	}

	for i := range want {
		if tabs[i] != want[i] {
			t.Errorf("attempt tab %d = %+v, want %+v", i+1, tabs[i], want[i])
		}
	}

	panels := regexp.MustCompile(`(?s)role="tabpanel".*?</pre>`).FindAllString(rows[0], -1)
	if len(panels) != 3 || !strings.Contains(panels[0], "try 1") || strings.Contains(panels[0], "try 2") || !strings.Contains(panels[2], "try 3") {
		t.Errorf("each attempt's output is not under its own tab:\n%s", rows[0])
	}

	if strings.Contains(visibleText(rows[0]), "(attempt 2/3)") {
		t.Error("the attempt lines the tabs replace are still drawn")
	}
}

// TestARunPageSaysATriedFailureWasTolerated: a try: that ate its step's failure is drawn as tolerated, not as the failure of a run that passed.
func TestARunPageSaysATriedFailureWasTolerated(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	transitionsPipeline(t, path, dir)
	page := runPage(t, path)

	tried := stepRows(page, "notify")
	if len(tried) == 0 || !strings.Contains(tried[0], `class="step tolerated`) {
		t.Fatalf("the try: is not drawn as tolerated:\n%s", page)
	}

	if head := stepHead(t, tried[0], "notify"); !strings.Contains(head, "⊘") || !strings.Contains(head, "tolerated") {
		t.Errorf("the tolerated try's row does not say so: %q", head)
	}

	if said := strings.Count(page, `command &#34;false&#34; failed`); said != 1 {
		t.Errorf("the tolerated failure's error is said %d times, want once, on the step that failed", said)
	}

	if strings.Contains(page, "failed at") {
		t.Error("the head of a passed run names a tolerated failure as where it failed")
	}
}

// TestRunsStepsSaysEachPassAndWhatItDecided: the CLI's step list names the pass and the routing outcome a resume replays, so a loop reads as a loop rather than a list with repeats.
func TestRunsStepsSaysEachPassAndWhatItDecided(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	routePipeline(t, path, filepath.Join(dir, "laps"))
	mustRun(t, "run", path, "--job", "build")

	_, target := webServerFor(t, path)

	runs, err := target.Store.ListRuns(t.Context(), "build", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}

	out := captureStdout(t, func() { mustRun(t, append([]string{"runs", "steps", runs[0].ID}, readArgs(path)...)...) })

	if !regexp.MustCompile(`(?m)^gate\s+1\s+failure\s*$`).MatchString(out) {
		t.Errorf("runs steps does not say the gate's first pass routed on failure:\n%s", out)
	}

	if !regexp.MustCompile(`(?m)^editor\s+2\s+success\s*$`).MatchString(out) {
		t.Errorf("runs steps does not say the editor ran a second pass:\n%s", out)
	}
}

// assertRoutedRow: the gate whose failure was routed says so, names where it went, and carries the decision itself, which is no longer in a banner above the transcript.
func assertRoutedRow(t *testing.T, page, routed string) {
	t.Helper()

	if !strings.Contains(routed, `class="step routed`) {
		t.Errorf("the gate whose failure was routed is not drawn as routed:\n%s", routed)
	}

	if head := stepHead(t, routed, "gate"); !strings.Contains(head, "↪") || !strings.Contains(head, "editor") {
		t.Errorf("the routed gate's row does not name where it sent the plan: %q", head)
	}

	if !strings.Contains(visibleText(routed), "route: gate --failure--> editor (visit 1/3)") {
		t.Errorf("the route decision is not on the row that made it:\n%s", visibleText(routed))
	}

	if strings.Contains(page, `class="runnotes"`) {
		t.Error("the route decision is still drawn above the transcript, away from the step that made it")
	}
}
