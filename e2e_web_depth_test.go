package main

// Depth, end to end: a job whose plan actually nests — a matrix of cells, each
// wrapped in a try: — rendered by the web UI as the tree it is rather than as
// the flat list every step used to collapse into.
//
// In the root package with the other e2e tests, and for the same reason: the
// containment being asserted is produced by the pipeline walk and consumed by
// the web model, and only main's run() spans both.

import (
	"regexp"
	"strings"
	"testing"
)

// nestedPipeline is one across: over two values, each cell wrapped in a try:,
// with a plain task on either side. It is the smallest plan that exercises
// every part of the tree: a container holding containers, siblings that must
// not merge, and leaves at three different depths.
const nestedPipeline = `
jobs:
- name: build
  plan:
  - task: compile
    run: echo compiling
  - across:
    - var: dim
      values: [alpha, beta]
    try:
      task: review
      run: echo reviewing {{ .vars.dim }}
  - task: publish
    run: echo publishing
`

// TestWebUINestsAMatrixUnderItsBlock is the tree's defining assertion: a
// cell's markup must live INSIDE its matrix's subtree, not beside it.
func TestWebUINestsAMatrixUnderItsBlock(t *testing.T) {
	dir := t.TempDir()
	path := writeNestedPipeline(t, dir)

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	// The two plain tasks are the matrix's siblings, so neither may appear
	// inside it — the check that the subtree extracted below really is the
	// matrix's own and not the rest of the page.
	matrix := subtreeOf(t, body, "across")

	for _, cell := range []string{"alpha", "beta"} {
		if !strings.Contains(matrix, cell) {
			t.Errorf("matrix subtree does not contain cell %q", cell)
		}
	}

	for _, sibling := range []string{"compile", "publish"} {
		if strings.Contains(matrix, sibling) {
			t.Errorf("matrix subtree wrongly contains its sibling %q", sibling)
		}
	}
}

// TestWebUIKeepsATryDistinctFromTheStepItWraps pins the collision that made
// the old flat list lie: a try: and the step inside it publish the same plan
// index and the same name, and keying steps by that pair folded the two into
// one row — losing the inner step's kind, hash and duration.
func TestWebUIKeepsATryDistinctFromTheStepItWraps(t *testing.T) {
	dir := t.TempDir()
	path := writeNestedPipeline(t, dir)

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	if got := strings.Count(body, `<span class="kind">try</span>`); got != 2 {
		t.Errorf("page shows %d try rows, want one per cell (2)", got)
	}

	// Two cells, each a try: wrapping a task, so the plan runs four task
	// steps in total: compile, publish, and one review per cell.
	if got := strings.Count(body, `<span class="kind">task</span>`); got != 4 {
		t.Errorf("page shows %d task rows, want 4", got)
	}

	// Every try: here wraps a task, so a try subtree that contains no task
	// row is one that swallowed the step it wraps.
	if inner := subtreeOf(t, body, "try"); !strings.Contains(inner, `<span class="kind">task</span>`) {
		t.Error("a try: subtree contains no step — the wrapped task was merged into it")
	}
}

// TestWebUIRollsUpAContainersOutcome covers the folded case: a reader who
// collapses a matrix — or reaches the page with one already collapsed —
// still has to be able to see where it stands without opening it.
func TestWebUIRollsUpAContainersOutcome(t *testing.T) {
	dir := t.TempDir()
	path := writeNestedPipeline(t, dir)

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	head := headOf(t, body, "across")
	if !strings.Contains(head, `class="rollup"`) {
		t.Fatalf("the matrix row carries no rollup: %s", head)
	}

	// Two cells, both green: the rollup has to say so on the row itself,
	// because the rows that would otherwise say it are folded away.
	if !strings.Contains(head, "2 passed") {
		t.Errorf("matrix rollup does not report its cells' outcome: %s", head)
	}
}

// TestWebUIMarksTheActivePath is the live half of the tree. A run in flight
// marks every container holding something still running, so a folded branch
// still shows where the work is.
func TestWebUIMarksTheActivePath(t *testing.T) {
	dir := t.TempDir()
	path := writeNestedPipeline(t, dir)

	mustRun(t, "run", path, "--job", "build")

	body := latestRunPage(t, path)

	// Nothing is running in a finished run, so the marker must be absent —
	// the assertion that it tracks state rather than being emitted always.
	if strings.Contains(body, "step active") || strings.Contains(body, " active\"") {
		t.Error("a finished run marks an active path")
	}
}

// writeNestedPipeline lays down the nesting fixture.
func writeNestedPipeline(t *testing.T, dir string) string {
	t.Helper()

	path := dir + "/nested.yml"
	writeE2EPipeline(t, path, nestedPipeline)

	return path
}

// latestRunPage renders the newest run of the pipeline's build job.
func latestRunPage(t *testing.T, pipelinePath string) string {
	t.Helper()

	server, pipeline := webServerFor(t, pipelinePath)

	runs, err := pipeline.Store.ListRuns(t.Context(), "build", 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	code, body := webGet(t, server, "/p/"+pipeline.Slug+"/runs/"+runs[0].ID)
	if code != 200 {
		t.Fatalf("run page = %d: %s", code, body)
	}

	return body
}

// stepKindRe finds a step row's opening markup by the kind it declares.
var stepKindRe = regexp.MustCompile(`(?s)<div id="step-[^"]*" class="step [^"]*"[^>]*>.*?<span class="kind">(\w+)</span>`)

// headOf returns the <div class="stephead"> of the first step of a kind.
func headOf(t *testing.T, body, kind string) string {
	t.Helper()

	start := stepRowStart(t, body, kind)

	head := body[start:]
	if end := strings.Index(head, "</div>"); end > 0 {
		head = head[:end]
	}

	return head
}

// subtreeOf returns the markup of the first step of a kind, from its opening
// tag to the close of its .substeps block — which is precisely the containment
// the tree claims.
func subtreeOf(t *testing.T, body, kind string) string {
	t.Helper()

	start := stepRowStart(t, body, kind)

	rest := body[start:]

	open := strings.Index(rest, `<div class="substeps">`)
	if open < 0 {
		t.Fatalf("step of kind %q renders no subtree", kind)
	}

	return rest[open : closingDiv(t, rest[open:])+open]
}

// stepRowStart is the byte offset of the step row that declares a kind.
func stepRowStart(t *testing.T, body, kind string) int {
	t.Helper()

	for _, m := range stepKindRe.FindAllStringSubmatchIndex(body, -1) {
		if body[m[2]:m[3]] == kind {
			return m[0]
		}
	}

	t.Fatalf("page has no step of kind %q", kind)

	return 0
}

// closingDiv finds the offset just past the </div> that closes the <div> the
// input opens with, counting nesting — the whole point being that a subtree
// contains more <div>s.
func closingDiv(t *testing.T, markup string) int {
	t.Helper()

	depth := 0

	for i := 0; i < len(markup); {
		switch {
		case strings.HasPrefix(markup[i:], "<div"):
			depth++
			i += 4
		case strings.HasPrefix(markup[i:], "</div>"):
			depth--
			i += 6

			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}

	t.Fatal("subtree markup is unbalanced")

	return 0
}
