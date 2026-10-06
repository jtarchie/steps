package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// A recorded run already names its job, so continuing one needs no --job even where the pipeline has several; not parallel, it captures stdout.
func TestAContinuationFindsItsOwnJob(t *testing.T) {
	dir := t.TempDir()
	attempts := filepath.Join(dir, "attempts.log")
	fixed := filepath.Join(dir, "fixed")
	path := filepath.Join(dir, "pipeline.yml")

	writePipelineFile(t, path, fmt.Sprintf(`
jobs:
- name: first
  plan:
  - task: idle
    run: "true"
- name: second
  plan:
  - task: setup
    run: "true"
  - task: fragile
    run: |
      echo attempt >> %s
      test -f %s
`, attempts, fixed))

	var err error

	_ = captureStdout(t, func() { err = Run([]string{"run", path, "--job", "second", "--keep-workspace"}) })
	if err == nil {
		t.Fatal("the fragile step was expected to fail")
	}

	writePipelineFile(t, fixed, "")

	failed := newestRun(t, path)

	_ = captureStdout(t, func() { err = Run([]string{"run", path, "--resume", failed, "--keep-workspace"}) })
	if err != nil {
		t.Fatalf("resume without --job: %v", err)
	}

	_ = captureStdout(t, func() {
		err = Run([]string{"run", path, "--replay", newestRun(t, path), "--from", "fragile", "--keep-workspace"})
	})
	if err != nil {
		t.Fatalf("replay without --job: %v", err)
	}

	data, err := os.ReadFile(attempts) //nolint:gosec // a path this test made
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Count(string(data), "attempt"); got != 3 {
		t.Errorf("the fragile step ran %d times, want 3: failed, resumed, replayed", got)
	}
}

// --rerun starts from the top, so pairing it with either way of continuing is refused rather than one flag quietly losing.
func TestARerunRefusesToBeAContinuationToo(t *testing.T) {
	path := flagFixture(t)

	for _, continuation := range []string{"--resume", "--replay"} {
		err := Run([]string{"run", path, "--rerun", "RUN", continuation, "OTHER"})
		if err == nil || !strings.Contains(err.Error(), "--rerun cannot be combined") {
			t.Errorf("--rerun with %s = %v, want it refused as a combination", continuation, err)
		}
	}
}

func newestRun(t *testing.T, path string) string {
	t.Helper()

	st, err := sqlite.OpenExisting(string(StatePath(path, "")), PipelineName(path))
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = st.Close() }()

	runs, err := st.ListRuns(t.Context(), "", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %v, %v", runs, err)
	}

	return runs[0].ID
}
