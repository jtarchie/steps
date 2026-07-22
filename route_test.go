package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestRunJobDoesNotRecordSucceededForInheritedFix: a task step referencing a
// top-level tasks: entry whose fix: is set only there (not inline on the
// step) must be treated as unskippable the same way merkle.PlanChains treats
// it at plan time — route.go's runtime stepForcesUnskippable used to check
// only the step's own literal Fix field, missing a fix: inherited via
// Config.ResolveTask, so it would incorrectly record a job_runs "succeeded"
// row for a chain that should never be treated as a cacheable success.
func TestRunJobDoesNotRecordSucceededForInheritedFix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipeline := `
agents:
- name: fixer
  source:
    endpoint: http://127.0.0.1:0/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

tasks:
- name: unit
  run: "true"
  fix: fixer

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	st, err := store.OpenStore(statePath(path))
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}
	defer func() { _ = st.Close() }()

	count, err := st.CountJobRuns(context.Background(), "build")
	if err != nil {
		t.Fatalf("count job_runs: %v", err)
	}

	if count != 0 {
		t.Errorf("job_runs rows for job %q = %d, want 0: a task whose fix: is inherited from a tasks: entry "+
			"must never be recorded as a reusable succeeded chain", "build", count)
	}
}
