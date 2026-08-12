package main

// End-to-end coverage for collected outputs on across: — `outputs:` on a
// matrix step means each cell's outputs are captured under the cell's own
// coordinates, so N cells share one declared artifact without clobbering
// each other, and a step after the block consumes the lot as ONE input.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndAcrossCollectsCellOutputs is the feature: two task cells write
// the SAME file name into the SAME declared output, and both survive — each
// under its own coordinate directory, consumed downstream as one artifact.
func TestEndToEndAcrossCollectsCellOutputs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: dim
      values: [alpha, beta]
    task: review
    inputs: []
    outputs: [findings]
    run: printf 'from {{ .vars.dim }}' > findings/report.txt
  - task: collect
    inputs: [findings]
    run: |
      cat findings/alpha/report.txt >> %[1]s
      echo >> %[1]s
      cat findings/beta/report.txt >> %[1]s
      echo >> %[1]s
`, log))

	mustRun(t, path)

	if got := readFileString(t, log); got != "from alpha\nfrom beta\n" {
		t.Errorf("collected artifact held %q, want each cell's file under its own directory", got)
	}
}

// TestEndToEndAcrossCollectDoesNotClobberConcurrently is the regression this
// feature exists for, reproduced with the interleaving that exposed it: a
// slow cell and a fast cell capturing the same declared output. Before
// collection, the leaf capture was remove-then-copy on ONE destination, so
// whichever cell finished last silently erased the other's files.
func TestEndToEndAcrossCollectDoesNotClobberConcurrently(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: dim
      values: [slow, fast]
    max_in_flight: 2
    task: review
    inputs: []
    outputs: [findings]
    run: |
      if [ "{{ .vars.dim }}" = "slow" ]; then sleep 2; fi
      printf 'x' > findings/report.txt
  - task: collect
    inputs: [findings]
    run: ls -1 findings/ >> %[1]s
`, log))

	mustRun(t, path)

	got := readFileString(t, log)
	for _, want := range []string{"slow", "fast"} {
		if !strings.Contains(got, want) {
			t.Errorf("collected artifact is missing cell %q; the later capture clobbered it:\n%s", want, got)
		}
	}
}

// TestEndToEndAcrossCollectsAgentCells proves the agent path captures through
// the same mapping tasks do: two agent cells, each told by its prompt to
// write the same file name, both land under their own coordinates.
func TestEndToEndAcrossCollectsAgentCells(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	log := filepath.Join(dir, "seen.log")

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if modelHasCalled(req, "write_file") {
			return says("done")
		}

		for _, dim := range []string{"alpha", "beta"} {
			if requestMentions(req, "the "+dim+" dimension") {
				return callsTool("write_file", map[string]any{"path": "findings/report.txt", "content": "from " + dim})
			}
		}

		return says("done")
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [write_file]

jobs:
- name: fan
  plan:
  - across:
    - var: dim
      values: [alpha, beta]
    max_in_flight: 2
    agent: reviewer
    inputs: []
    outputs: [findings]
    prompt: "Review the {{ .vars.dim }} dimension; write findings/report.txt"
  - task: collect
    inputs: [findings]
    run: |
      cat findings/alpha/report.txt >> %[2]s
      echo >> %[2]s
      cat findings/beta/report.txt >> %[2]s
      echo >> %[2]s
`, fake.URL+"/v1/", log))

	mustRun(t, path)

	if got := readFileString(t, log); got != "from alpha\nfrom beta\n" {
		t.Errorf("collected artifact held %q, want each agent cell's file under its own directory", got)
	}
}

// TestEndToEndAcrossCollectFailedCellContributesNothing: a try:-tolerated
// cell that died before writing leaves no directory, and the consumer walks
// what survived — the same absence-tolerance the rest of the fan-out story
// has. The failed cell must not leave a half-written directory behind.
func TestEndToEndAcrossCollectFailedCellContributesNothing(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: dim
      values: [good, bad]
    try:
      task: review
      inputs: []
      outputs: [findings]
      run: |
        printf 'from {{ .vars.dim }}' > findings/report.txt
        test "{{ .vars.dim }}" = good
  - task: collect
    inputs: [findings]
    run: ls -1 findings/ >> %[1]s
`, log))

	mustRun(t, path)

	got := readFileString(t, log)
	if !strings.Contains(got, "good") {
		t.Errorf("the succeeding cell's directory is missing:\n%s", got)
	}

	if strings.Contains(got, "bad") {
		t.Errorf("the failed cell left a directory behind; capture must only run on success:\n%s", got)
	}
}

// TestEndToEndAcrossCollectCellsRerun pins the caching answer: a task cell
// that produces outputs is not cell-cacheable, because a skipped cell
// captures nothing and the consumer would read a hole where its contribution
// was. Both runs must see both cells execute AND both files present.
func TestEndToEndAcrossCollectCellsRerun(t *testing.T) {
	dir := t.TempDir()
	ran := filepath.Join(dir, "ran.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - across:
    - var: dim
      values: [alpha, beta]
    task: review
    inputs: []
    outputs: [findings]
    run: |
      echo {{ .vars.dim }} >> %[1]s
      printf 'x' > findings/report.txt
  - task: collect
    inputs: [findings]
    run: test -s findings/alpha/report.txt && test -s findings/beta/report.txt
`, ran))

	mustRun(t, path)
	assertLineCount(t, ran, 2)

	mustRun(t, path)
	assertLineCount(t, ran, 4)
}

// TestEndToEndAcrossCollectFromFile is the shape the feature was built for:
// one step decides the width, the matrix fans out over it, and everything the
// cells produced comes back as one artifact.
func TestEndToEndAcrossCollectFromFile(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "seen.log")

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [dims]
    run: printf '["alpha","beta"]' > dims/index.json
  - across:
    - var: dim
      from_file: dims/index.json
    max_in_flight: 2
    task: review
    inputs: [dims]
    outputs: [findings]
    run: printf 'from {{ .vars.dim }}' > findings/report.txt
  - task: collect
    inputs: [findings]
    run: |
      cat findings/alpha/report.txt >> %[1]s
      echo >> %[1]s
      cat findings/beta/report.txt >> %[1]s
      echo >> %[1]s
`, log))

	mustRun(t, path)

	if got := readFileString(t, log); got != "from alpha\nfrom beta\n" {
		t.Errorf("collected artifact held %q, want both cells' files", got)
	}
}

// TestEndToEndAcrossCollectRejectsAPathHostileItem: a from_file: value on a
// collecting matrix becomes a directory name, and the items are often
// model-authored — so a value that cannot name a directory fails the block
// naming the value, before any cell runs.
func TestEndToEndAcrossCollectRejectsAPathHostileItem(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
workspace:
  strategy: copy

jobs:
- name: fan
  plan:
  - task: scan
    inputs: []
    outputs: [dims]
    run: printf '["ok","../escape"]' > dims/index.json
  - across:
    - var: dim
      from_file: dims/index.json
    task: review
    inputs: [dims]
    outputs: [findings]
    run: "true"
`)

	err := run([]string{path})
	if err == nil {
		t.Fatal("the run succeeded with a traversal value naming a collected directory")
	}

	if !strings.Contains(err.Error(), "cannot name a directory") {
		t.Errorf("error = %v, want the path-segment refusal naming the item", err)
	}
}
