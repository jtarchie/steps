package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestExplainNamesWhyAStepWouldRun: `steps plan` says a fix: task can never be cached, and that a plain task upstream of it is uncacheable only because of its chain.
func TestExplainNamesWhyAStepWouldRun(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.yml")

	err := os.WriteFile(path, []byte(`
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system: You fix failing checks.
  tools: [read_file]

jobs:
- name: build
  plan:
  - task: plain
    run: "true"
  - task: check
    run: "true"
    fix: fixer
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	rows, err := Explain(context.Background(), cfg, &cfg.Jobs[0], nil, st)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	reasons := map[string]string{}
	for _, row := range rows {
		reasons[row.Name] = row.Reason
	}

	for name, want := range map[string]string{"plain": "chain uncacheable", "check": "fix: agent"} {
		if reasons[name] != want {
			t.Errorf("%s: reason %q, want %q (all rows: %+v)", name, reasons[name], want, rows)
		}
	}
}

// The planner restarts a node's StepIndex at zero after every get, so a reason looked up by it judged a step after a get as whichever step sat at that index from the top: the fix: task below read "not yet run", the reason of the task before the get.
func TestExplainJudgesAStepAfterAGetAsItself(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.yml")

	err := os.WriteFile(path, []byte(`
agents:
- name: fixer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system: You fix failing checks.
  tools: [read_file]

resource_types:
- name: probe
  config:
    check: printf '[{"ref":"v1"}]'
    in: "true"

resources:
- name: items
  type: probe
  source: {}

jobs:
- name: build
  plan:
  - task: first
    run: "true"
  - get: items
  - task: check
    run: "true"
    fix: fixer
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	rows, err := Explain(context.Background(), cfg, &cfg.Jobs[0], nil, st)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	for _, row := range rows {
		if row.Name == "check" {
			if row.Reason != "fix: agent" || row.StepIndex != 2 {
				t.Fatalf("check after a get: reason %q at index %d, want %q at 2 (all rows: %+v)", row.Reason, row.StepIndex, "fix: agent", rows)
			}

			return
		}
	}

	t.Fatalf("no row for check: %+v", rows)
}

// `steps plan` prints a node hash at a width a person compares by eye, and a hash already that short whole.
func TestShortHashAbbreviatesOnlyWhatIsLonger(t *testing.T) {
	t.Parallel()

	full := strings.Repeat("ab", 32)
	if got := shortHash(full); got != full[:12] {
		t.Errorf("shortHash(64 chars) = %q, want %q", got, full[:12])
	}

	if got := shortHash("abc"); got != "abc" {
		t.Errorf("shortHash(%q) = %q, want it whole", "abc", got)
	}
}

// A block holding an uncacheable step makes its chain uncacheable, and `steps
// plan` said "not yet run" for every step of such a job however many times it
// had run — the planner knew and the explanation did not, so a job that never
// caches read as one that simply had not been tried.
func TestExplainSaysAContainerMakesTheChainUncacheable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipe.yml")

	err := os.WriteFile(path, []byte(`
resource_types:
- name: sink
  config:
    out: "echo '{\"v\": \"1\"}'"
resources:
- name: out
  type: sink
  source: {}
jobs:
- name: build
  plan:
  - task: first
    run: "true"
  - in_parallel:
      steps:
      - task: branch
        run: "true"
      - put: out
  - task: last
    run: "true"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	rows, err := Explain(context.Background(), cfg, &cfg.Jobs[0], nil, st)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	reasons := map[string]string{}
	for _, row := range rows {
		reasons[row.Kind+" "+row.Name] = row.Reason
	}

	for name, want := range map[string]string{
		"task first":   "chain uncacheable",
		"in_parallel ": "in_parallel block: put step",
		"task last":    "chain uncacheable",
	} {
		if reasons[name] != want {
			t.Errorf("%s: reason %q, want %q (all rows: %+v)", name, reasons[name], want, rows)
		}
	}
}

// `steps plan` once resolved through its own copy of RunJob's setup, and the
// copy drifted: plan judged versions the run would never build. Whatever plan
// says of a step, the run that follows does — for a version: every job one
// build into its backlog (the cursor decides what is left) and for a job
// already cached (the chain index decides what skips).
func TestExplainVerdictIsWhatTheNextRunDoes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	everyLog := filepath.Join(dir, "every.txt")
	latestLog := filepath.Join(dir, "latest.txt")

	writeFixture(t, path, fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: printf '[{"n":"v1"},{"n":"v2"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: listing
  source: {}
jobs:
- name: every
  plan:
  - get: items
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
- name: latest
  plan:
  - get: items
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, everyLog, latestLog))

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	for index, logPath := range []string{everyLog, latestLog} {
		job := &cfg.Jobs[index]

		err = RunJob(context.Background(), cfg, job, nil, provider, st, false)
		if err != nil {
			t.Fatalf("%s: first RunJob: %v", job.Name, err)
		}

		work, ran := explainThenRun(t, cfg, job, provider, st, logPath, "work")
		if len(work) != 1 || work[0].WouldSkip == ran {
			t.Errorf("%s: plan said %+v of task work; the next run ran it: %v", job.Name, work, ran)
		}
	}
}

// explainThenRun returns Explain's rows for step, then runs job and reports
// whether step ran — told by it appending to logPath.
func explainThenRun(
	t *testing.T, cfg *config.Config, job *config.Job, provider workspace.Provider, st store.Store, logPath, step string,
) ([]ExplainRow, bool) {
	t.Helper()

	lines := func() int {
		data, _ := os.ReadFile(logPath) //nolint:gosec // a t.TempDir()-scoped file the test wrote itself

		return len(strings.Fields(string(data)))
	}

	rows, err := Explain(context.Background(), cfg, job, nil, st)
	if err != nil {
		t.Fatalf("%s: Explain: %v", job.Name, err)
	}

	var matched []ExplainRow

	for _, row := range rows {
		if row.Name == step {
			matched = append(matched, row)
		}
	}

	before := lines()

	err = RunJob(context.Background(), cfg, job, nil, provider, st, false)
	if err != nil {
		t.Fatalf("%s: RunJob after Explain: %v", job.Name, err)
	}

	return matched, lines() > before
}
