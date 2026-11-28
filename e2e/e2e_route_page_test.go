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

// stepRows is every row on a run page drawn for a step named name, in page order, each from its opening div to the next row's.
func stepRows(page, name string) []string {
	starts := regexp.MustCompile(`<div id="step-\d+-`+regexp.QuoteMeta(name)+`"`).FindAllStringIndex(page, -1)
	rows := make([]string, 0, len(starts))

	for _, at := range starts {
		rest := page[at[0]:]

		end := strings.Index(rest[1:], `<div id="step-`)
		if end < 0 {
			end = len(rest) - 1
		}

		rows = append(rows, rest[:end+1])
	}

	return rows
}

// TestARunPageSaysWhereARouteSentThePlan: a failure a to: route consumed is drawn as a branch the plan took, on the row that took it, and the steps it sent the plan back through say which pass they are. It used to be a red ✗ on a run that passed, with the decision in a banner above the transcript, unplaced and untimed.
func TestARunPageSaysWhereARouteSentThePlan(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := pipelinePath(t, dir)
	routePipeline(t, path, filepath.Join(dir, "laps"))
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

	gates, editors := stepRows(page, "gate"), stepRows(page, "editor")
	if len(gates) != 2 || len(editors) != 2 {
		t.Fatalf("want two laps, got %d gate and %d editor rows", len(gates), len(editors))
	}

	assertRoutedRow(t, page, gates[0])

	if head := stepHead(t, editors[1], "editor"); !strings.Contains(head, "pass 2") {
		t.Errorf("the editor the route sent the plan back to does not say which pass it is: %q", head)
	}

	if head := stepHead(t, editors[0], "editor"); strings.Contains(head, "pass") {
		t.Errorf("the first editor claims a pass number: %q", head)
	}

	if !strings.Contains(gates[1], `class="step passed`) {
		t.Errorf("the second gate is not drawn passed:\n%s", gates[1])
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
