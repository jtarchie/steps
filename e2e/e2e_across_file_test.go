package e2e

// End-to-end coverage for `across: from_file:` — a matrix whose width one step
// decides and the next step fans out over.
//
// The whole point is that nothing carries the list but an ordinary artifact:
// the producer declares `outputs:`, writes JSON, and the matrix reads it. So
// these run with no model and no store — a task writes the file, tasks fan out
// over it, and what they did is read back off the run.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndAcrossFromFileFansOutOverAWrittenList is the feature: two cells
// exist because a step said so mid-run, each named for its value and recorded
// as its own node.
func TestEndToEndAcrossFromFileFansOutOverAWrittenList(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && printf '["alpha","beta"]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    task: "investigate-{{ .vars.item }}"
    inputs: [findings]
    run: echo {{ .vars.item }} >> %[1]s
`, log))

	mustRun(t, path)

	// One cell per item, each under the name its value gave it.
	nodes := storeNodes(t, path)
	for _, name := range []string{"investigate-alpha", "investigate-beta"} {
		assertSucceeded(t, nodes, "task", name)
	}

	// And each actually ran, in declaration order.
	if got := readFileString(t, log); got != "alpha\nbeta\n" {
		t.Errorf("cells ran %q, want alpha then beta", got)
	}
}

// TestEndToEndAcrossFromFileProducerAlwaysReruns is the cache property the
// design leans on, checked end to end rather than argued.
//
// A chain through an across: block is unskippable, so the producing task
// re-runs on every run — which is what guarantees the file the axis reads is
// the file THIS run wrote. Were the producer cacheable, a second run would
// expand over whatever the first left behind (or nothing at all, under
// isolation), and this test would see one line instead of two.
func TestEndToEndAcrossFromFileProducerAlwaysReruns(t *testing.T) {
	dir := t.TempDir()
	produced := filepath.Join(dir, "produced.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: |
      echo scanned >> %[1]s
      mkdir -p findings
      printf '["alpha"]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    task: "investigate-{{ .vars.item }}"
    inputs: [findings]
    run: "true"
`, produced))

	mustRun(t, path)
	assertLineCount(t, produced, 1)

	// Same pipeline, second run: the producer must run again.
	mustRun(t, path)
	assertLineCount(t, produced, 2)
}

// TestEndToEndAcrossFromFileEmptyListRunsNothing pins the decided answer to an
// empty array: zero cells, said out loud, and the plan carries on. "The scan
// found nothing" is a legitimate success, and an author who wants it to fail
// asserts that where the file is written.
func TestEndToEndAcrossFromFileEmptyListRunsNothing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && printf '[]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    task: "investigate-{{ .vars.item }}"
    inputs: [findings]
    run: echo {{ .vars.item }} >> %[1]s
  - task: after
    inputs: []
    run: echo "the plan continued" >> %[1]s
`, log))

	mustRun(t, path)

	// No cell ran, and the step after the block did.
	if got := readFileString(t, log); got != "the plan continued\n" {
		t.Errorf("log = %q, want only the step after the block", got)
	}

	assertSucceeded(t, storeNodes(t, path), "task", "after")
}

// TestEndToEndAcrossFromFileProductWithAStaticAxis proves full symmetry: a
// file axis is an ordinary axis, so it multiplies with a static one.
func TestEndToEndAcrossFromFileProductWithAStaticAxis(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && printf '["alpha","beta"]' > findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    - var: mode
      values: [fast, thorough]
    task: check
    inputs: [findings]
    run: echo {{ .vars.item }}/{{ .vars.mode }} >> %[1]s
`, log))

	mustRun(t, path)

	// Row-major, the last axis varying fastest — the same order a matrix of
	// two static axes produces.
	want := "alpha/fast\nalpha/thorough\nbeta/fast\nbeta/thorough\n"
	if got := readFileString(t, log); got != want {
		t.Errorf("cells ran:\n%s\nwant:\n%s", got, want)
	}

	assertSucceeded(t, storeNodes(t, path), "task", "check [item=alpha mode=fast]")
}

// TestEndToEndAcrossFromFileRejectsBadContent proves a wrong shape fails the
// block naming the file, rather than expanding into something surprising. The
// array is written during the run, so this is the likeliest failure there is.
func TestEndToEndAcrossFromFileRejectsBadContent(t *testing.T) {
	for _, tc := range []struct{ name, content, wantErr string }{
		{name: "objects", content: `[{"id":"a"}]`, wantErr: "must hold a JSON array of strings"},
		{name: "not json", content: `alpha`, wantErr: "must hold a JSON array of strings"},
		{name: "missing file", content: "", wantErr: "could not read findings/items.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			write := fmt.Sprintf("mkdir -p findings && printf %s > findings/items.json", quoteForShell(tc.content))
			if tc.content == "" {
				write = "mkdir -p findings" // the directory, but no file in it
			}

			path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: %[1]s
  - across:
    - var: item
      from_file: findings/items.json
    task: work
    inputs: [findings]
    run: "true"
`, write))

			err := cli.Run([]string{path})
			if err == nil {
				t.Fatal("the run succeeded with an unusable axis file")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// quoteForShell wraps s in single quotes for a printf argument, escaping any
// single quote within it.
func quoteForShell(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
