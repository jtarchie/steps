package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

const retireKeepJob = `
jobs:
- name: keep
  plan:
  - task: compile
    inputs: []
    run: "true"
`

const retireBothJobs = `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"

resources:
- name: repo
  type: dummy
  source: {}
` + retireKeepJob + `
- name: drop
  plan:
  - get: repo
  - task: compile
    inputs: []
    run: "true"
`

// TestASetWithoutAJobForgetsWhatItRecorded: a job and a resource a set stops naming take their runs and versions with them (a deliberate divergence: Concourse marks them inactive and keeps every build), and the job the set still names keeps its own.
func TestASetWithoutAJobForgetsWhatItRecorded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("STEPS_ENCRYPTION_KEY", testEncryptionKey)
	t.Setenv("HOME", dir)

	path := writePipeline(t, dir, retireBothJobs)

	served := startWebFor(t, path, "--interval", "1h")
	defer served.stop(t)

	name := cli.PipelineName(path)

	served.trigger(t, name, "keep")
	served.trigger(t, name, "drop")
	waitForQueueSuccess(t, served.state, name, 2)

	if runs, versions := retireRecorded(t, served.state, name, "drop", "repo"); runs == 0 || versions == 0 {
		t.Fatalf("before the set, drop has %d runs and repo %d versions; the fixture recorded nothing to forget", runs, versions)
	}

	err := os.WriteFile(path, []byte(retireKeepJob), 0o600)
	if err != nil {
		t.Fatalf("rewrite pipeline: %v", err)
	}

	served.set(t, name, path)

	// The poller adopts a set from its own goroutine, between polls, so the rows go a moment after the set returns.
	deadline := time.Now().Add(30 * time.Second)

	for {
		runs, versions := retireRecorded(t, served.state, name, "drop", "repo")
		if runs == 0 && versions == 0 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("after a set without them, drop still has %d runs and repo %d versions", runs, versions)
		}

		time.Sleep(50 * time.Millisecond)
	}

	if runs, _ := retireRecorded(t, served.state, name, "keep", "repo"); runs != 1 {
		t.Errorf("keep has %d runs after the set, want the one it recorded: the set still names it", runs)
	}
}

func retireRecorded(t *testing.T, state, name, job, resource string) (int, int) {
	t.Helper()

	st, err := sqlite.OpenStore(state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), job, 0)
	if err != nil {
		t.Fatalf("ListRuns(%s): %v", job, err)
	}

	versions, err := st.VersionOrders(t.Context(), resource)
	if err != nil {
		t.Fatalf("VersionOrders(%s): %v", resource, err)
	}

	return len(runs), len(versions)
}
