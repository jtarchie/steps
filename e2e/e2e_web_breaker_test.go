package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestAHeldJobRunsWhenAPersonTriggersIt: the breaker stops a job that keeps failing from being triggered AUTOMATICALLY — the page has always said "will not auto-trigger" — so a person pressing Trigger on a held job, most likely to try a fix, gets a run. It used to be accepted, queued, and then dropped at claim as "skipped" with nothing on screen saying why.
func TestAHeldJobRunsWhenAPersonTriggersIt(t *testing.T) {
	dir := t.TempDir()
	tally := filepath.Join(dir, "ran.txt")
	path := filepath.Join(dir, "held.yml")

	err := os.WriteFile(path, []byte(`
jobs:
  - name: build
    max_consecutive_failures: 1
    plan:
      - task: append
        inputs: []
        run: echo ran >> `+tally+`
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	served := startWebFor(t, path)
	defer served.stop(t)

	name := cli.PipelineName(path)

	st, err := sqlite.OpenExisting(served.state, name)
	if err != nil {
		t.Fatalf("open state store: %v", err)
	}

	held, _, err := st.RecordJobOutcome(t.Context(), "build", false, 1)
	_ = st.Close()

	if err != nil || !held {
		t.Fatalf("RecordJobOutcome = held %v, %v: the fixture did not hold the job", held, err)
	}

	served.trigger(t, name, "build")

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && countLines(t, tally) < 1 {
		time.Sleep(20 * time.Millisecond)
	}

	if got := countLines(t, tally); got != 1 {
		t.Fatalf("a manual trigger on a held job ran %d times, want 1", got)
	}
}
