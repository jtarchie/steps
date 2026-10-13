package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store/sqlite"
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
