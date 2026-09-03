package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestResumeUnderIsolationContinuesWhereItStopped is the gap issue #59
// describes: max_in_flight: REQUIRES a workspace: strategy, and --resume used
// to refuse one — so the concurrent fan-out, the shape most likely to die
// halfway and the most expensive to repeat, was the one shape locked out of
// recovery.
func TestResumeUnderIsolationContinuesWhereItStopped(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ran.log")

	pipeline := func(secondFails bool) string {
		fail := "true"
		if secondFails {
			fail = "false"
		}

		return fmt.Sprintf(`
workspace:
  strategy: copy

resources:
- name: seed
  type: mock
  source: {}

resource_types:
- name: mock
  config:
    check: 'echo ''[{"ref":"v1"}]'''
    in: 'echo seeded > seeded.txt'

jobs:
- name: build
  plan:
  - get: seed
  - task: first
    inputs: [seed]
    outputs: [made]
    run: |
      echo first >> %[1]s
      mkdir -p made && echo produced > made/thing.txt
  - task: second
    inputs: [made]
    run: |
      echo second >> %[1]s
      test -s made/thing.txt
      %[2]s
`, log, fail)
	}

	// First run dies on `second`; `first` and the get have completed.
	path := writePipeline(t, dir, pipeline(true))

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("the run succeeded; the fixture needs it to fail at the second step")
	}

	assertLineCount(t, log, 2) // first + second (which failed)

	runID := resumeIDFrom(t, path)
	if runID == "" {
		t.Fatal("no run was recorded, so nothing could be resumed — the isolating build must report its root")
	}

	// Same pipeline, second step now passes. Resuming must NOT re-run `first`
	// (its output is already in the tree) and must find made/thing.txt there.
	path = writePipeline(t, dir, pipeline(false))

	err = cli.Run([]string{path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	// first ran once, second ran twice: the resume continued rather than
	// restarting, and the artifact from the completed step survived.
	got := readFileString(t, log)
	if n := strings.Count(got, "first"); n != 1 {
		t.Errorf("`first` ran %d times, want 1 — a resume must not repeat completed steps:\n%s", n, got)
	}

	if n := strings.Count(got, "second"); n != 2 {
		t.Errorf("`second` ran %d times, want 2:\n%s", n, got)
	}
}

// resumeIDFrom reads the recorded run id for a pipeline's state database.
func resumeIDFrom(t *testing.T, pipelinePath string) string {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	var id string

	err := db.QueryRowContext(t.Context(), `SELECT id FROM runs ORDER BY started_at DESC LIMIT 1`).Scan(&id)
	if err != nil {
		t.Logf("no run row: %v", err)

		return ""
	}

	return id
}
