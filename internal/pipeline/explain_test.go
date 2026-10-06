package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// TestExplainNamesWhyAStepWouldRun: `steps plan` says a fix: task can never be cached, and says nothing of the kind about a plain task.
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

	for name, want := range map[string]string{"plain": "not yet run", "check": "fix: agent"} {
		if reasons[name] != want {
			t.Errorf("%s: reason %q, want %q (all rows: %+v)", name, reasons[name], want, rows)
		}
	}
}
