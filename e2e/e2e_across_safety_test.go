package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/config"
)

// TestEndToEndAcrossShellquoteKeepsAWrittenItemInert is the from_file: axis
// fed by a model: an item is data, and `| shellquote` is what keeps it data
// once it is spliced into a run: line that sh -c reads.
func TestEndToEndAcrossShellquoteKeepsAWrittenItemInert(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")
	pwned := filepath.Join(dir, "pwned")

	items, err := json.Marshal([]string{"$(touch " + pwned + ")", "a b; touch " + pwned, "it's"})
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "items.json")

	err = os.WriteFile(source, items, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && cp %[1]s findings/items.json
  - across:
    - var: item
      from_file: findings/items.json
    task: investigate
    inputs: [findings]
    run: printf '%%s\n' {{ .vars.item | shellquote }} >> %[2]s
`, source, log))

	mustRun(t, path)

	_, err = os.Stat(pwned)
	if err == nil {
		t.Fatal("an across item ran as shell: the pwned marker exists")
	}

	want := "$(touch " + pwned + ")\na b; touch " + pwned + "\nit's\n"
	if got := readFileString(t, log); got != want {
		t.Errorf("cells printed %q, want each item verbatim %q", got, want)
	}
}

// TestEndToEndAcrossRefusesAProductOverTheCellCap: each axis is under its own
// cap, but two model-written lists multiply, and the product is what costs.
func TestEndToEndAcrossRefusesAProductOverTheCellCap(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	side := 40
	if side*side <= config.MaxAcrossCells || side > config.MaxAcrossItems {
		t.Fatalf("side %d no longer exceeds the cell cap %d with each axis legal", side, config.MaxAcrossCells)
	}

	values := make([]string, side)
	for i := range values {
		values[i] = fmt.Sprintf("v%d", i)
	}

	items, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(dir, "items.json")

	err = os.WriteFile(source, items, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [findings]
    run: mkdir -p findings && cp %[1]s findings/a.json && cp %[1]s findings/b.json
  - across:
    - var: a
      from_file: findings/a.json
    - var: b
      from_file: findings/b.json
    task: work
    inputs: [findings]
    run: echo ran >> %[2]s
`, source, log))

	err = cli.Run([]string{path})
	if err == nil {
		t.Fatal("a matrix over the cell cap ran")
	}

	want := fmt.Sprintf("%d cells, above the limit of %d", side*side, config.MaxAcrossCells)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to contain %q", err, want)
	}

	_, err = os.Stat(log)
	if err == nil {
		t.Error("a cell ran before the matrix was refused")
	}
}
