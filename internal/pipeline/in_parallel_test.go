package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	st "github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// openTestStore returns a Store backed by a fresh temp database.
func openTestStore(t *testing.T) *st.Store {
	t.Helper()

	store, err := st.OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}

func TestRunInParallelAllSucceed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: lint
        run: echo lint >> %s
      - task: test
        run: echo test >> %s
`, marker, marker)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t)

	err = RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, store, true)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	// Both tasks should have run — check the marker file.
	data, err := os.ReadFile(marker) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatal(err)
	}

	content := strings.TrimSpace(string(data))
	if !strings.Contains(content, "lint") || !strings.Contains(content, "test") {
		t.Errorf("marker = %q, want both 'lint' and 'test'", content)
	}
}

func TestRunInParallelOneFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
jobs:
- name: build
  plan:
  - in_parallel:
      fail_fast: false
      steps:
      - task: failer
        run: exit 1
      - task: ok
        run: echo ok
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t)

	err = RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, store, true)
	if err == nil {
		t.Error("RunJob: expected error from failing child with fail_fast: false, got nil")
	}
}

func TestRunInParallelFailFast(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
jobs:
- name: build
  plan:
  - in_parallel:
      fail_fast: true
      steps:
      - task: fast-fail
        run: exit 1
      - task: slow
        run: sleep 10
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, store, true)
	if err == nil {
		t.Error("RunJob: expected error from failing child with fail_fast: true, got nil")
	}

	// If we reached here quickly (before the 10s sleep), fail_fast cancellation worked.
	if ctx.Err() != nil {
		t.Fatal("test context expired; fail_fast may not have cancelled the slow child")
	}
}

func TestRunInParallelLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
      limit: 1
      steps:
      - task: one
        run: echo start1 >> %s; sleep 0.2; echo end1 >> %s
      - task: two
        run: echo start2 >> %s; sleep 0.2; echo end2 >> %s
`, marker, marker, marker, marker)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t)

	err = RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, store, true)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	assertSerialized(t, marker)
}

func assertSerialized(t *testing.T, marker string) {
	t.Helper()

	data, err := os.ReadFile(marker) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatal(err)
	}

	content := strings.TrimSpace(string(data))
	lines := strings.Split(content, "\n")

	// With limit: 1, start1 and end1 should appear before start2.
	idx1Start := indexOf(lines, "start1")
	idx1End := indexOf(lines, "end1")
	idx2Start := indexOf(lines, "start2")
	idx2End := indexOf(lines, "end2")

	if idx1Start < 0 || idx1End < 0 || idx2Start < 0 || idx2End < 0 {
		t.Fatalf("marker missing expected lines: %v", lines)
	}

	if idx1End > idx2Start {
		t.Errorf("limit: 1 failed to serialize execution: end1 at %d, start2 at %d", idx1End, idx2Start)
	}
}

func indexOf(lines []string, s string) int {
	for i, line := range lines {
		if line == s {
			return i
		}
	}
	return -1
}

func TestRunInParallelNested(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - in_parallel:
          steps:
          - task: a1
            run: echo a1 >> %s
          - task: a2
            run: echo a2 >> %s
      - in_parallel:
          steps:
          - task: b1
            run: echo b1 >> %s
          - task: b2
            run: echo b2 >> %s
`, marker, marker, marker, marker)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	store := openTestStore(t)

	err = RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, store, true)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	// All four nested tasks should have run.
	data, err := os.ReadFile(marker) //nolint:gosec // test-owned temp file
	if err != nil {
		t.Fatal(err)
	}

	content := strings.TrimSpace(string(data))
	for _, want := range []string{"a1", "a2", "b1", "b2"} {
		if !strings.Contains(content, want) {
			t.Errorf("marker missing %q; got %q", want, content)
		}
	}
}

func TestUnskippableReasonInParallel(t *testing.T) {
	t.Parallel()

	step := config.Step{
		InParallel: &config.InParallelSpec{
			Steps: []config.Step{{Task: "lint", Run: "echo hi"}},
		},
	}

	reason := unskippableReason(step)
	if reason != "in_parallel step" {
		t.Errorf("unskippableReason = %q, want %q", reason, "in_parallel step")
	}
}

func TestStepLabelInParallel(t *testing.T) {
	t.Parallel()

	step := config.Step{
		InParallel: &config.InParallelSpec{
			Steps: []config.Step{{Task: "lint", Run: "echo hi"}},
		},
	}

	label := stepLabel(3, step)
	if label != "step 3 (in_parallel)" {
		t.Errorf("stepLabel = %q, want %q", label, "step 3 (in_parallel)")
	}
}

func TestExecutedStepNameInParallel(t *testing.T) {
	t.Parallel()

	step := config.Step{
		InParallel: &config.InParallelSpec{
			Steps: []config.Step{{Task: "lint", Run: "echo hi"}},
		},
	}

	name := executedStepName(step)
	if name != "" {
		t.Errorf("executedStepName = %q, want empty", name)
	}
}

func TestResolveStepImageInParallel(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	step := config.Step{
		InParallel: &config.InParallelSpec{
			Steps: []config.Step{{Task: "lint", Run: "echo hi"}},
		},
	}

	img, err := resolveStepImage(cfg, step)
	if err != nil {
		t.Fatalf("resolveStepImage: %v", err)
	}

	if img != "" {
		t.Errorf("resolveStepImage = %q, want empty", img)
	}
}
