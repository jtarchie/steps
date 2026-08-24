package pipeline

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// Why a chain stopped being cacheable is real, documented behavior that used
// to produce no output at all — the user saw steps re-running and had to infer
// the rule from the source.
func TestUnskippableReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		step config.Step
		want string
	}{
		{name: "put", step: config.Step{Put: "release"}, want: "put step"},
		{name: "agent", step: config.Step{Agent: "reviewer"}, want: "agent step"},
		{name: "when", step: config.Step{Task: "t", When: &config.WhenSpec{Run: "true"}}, want: "when: guard"},
		{name: "to", step: config.Step{Task: "t", To: map[string]string{"success": "next"}}, want: "to: routing"},
		{name: "plain task", step: config.Step{Task: "t", Run: "true"}, want: ""},
		{name: "get", step: config.Step{Get: "repo"}, want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := unskippableReason(test.step)
			if got != test.want {
				t.Errorf("unskippableReason() = %q, want %q", got, test.want)
			}
		})
	}
}

// A when: guard or a to: route disables caching for the whole rest of the
// chain, so the reason is reported by the step that first does it.
func TestFoldStepUnskippableReportsOnce(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	step := config.Step{Task: "t", To: map[string]string{"success": "next"}}

	unskippable, err := foldStepUnskippable(t.Context(), cfg, step, false)
	if err != nil {
		t.Fatal(err)
	}

	if !unskippable {
		t.Error("a to: step must make its chain unskippable")
	}

	// Already unskippable: still unskippable, and nothing new to announce.
	unskippable, err = foldStepUnskippable(t.Context(), cfg, step, true)
	if err != nil {
		t.Fatal(err)
	}

	if !unskippable {
		t.Error("an already-unskippable chain must stay unskippable")
	}
}

// A `get: version: every` whose check returns [] fans out zero builds and the
// job exits 0 — the same thing a fully-successful job looks like. That is how
// the self-build pipeline (experiments/self-build) spent runs doing literally
// nothing after its story directory was deleted: "no new versions" and "the
// source is gone" were indistinguishable from the outside. A resource that has
// never had a version at all is an input that can bind NOTHING — the report
// must name it as blocking, not read as idle.
func TestGetNoVersionsIsAnnounced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")
	marker := filepath.Join(dir, "ran.txt")

	err := os.WriteFile(path, []byte(`
resource_types:
- name: empty
  config:
    check: printf '[]'
    in: "true"

resources:
- name: thing
  type: empty
  source: {}

jobs:
- name: build
  plan:
  - get: thing
    version: every
  - task: work
    run: touch `+marker+`
  - task: more
    run: "true"
`), 0o600)
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

	st, err := store.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var runErr error

	out := captureStdout(t, func() {
		runErr = RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, st, false)
	})

	if runErr != nil {
		t.Fatalf("RunJob = %v; an empty check is idle, not a failure", runErr)
	}

	// The premise: nothing downstream ran. If this ever stops holding the
	// message below is a lie, not just noise.
	_, statErr := os.Stat(marker)
	if statErr == nil {
		t.Fatal("the task after an empty get ran; this test no longer covers the silent case")
	}

	if !strings.Contains(out, "get: thing cannot build; no versions exist for: thing") {
		t.Errorf("stdout must name the resource that blocks the build; got:\n%s", out)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what
// fn printed. Callers must not use t.Parallel().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = writer

	fn()

	_ = writer.Close()

	os.Stdout = orig

	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// A plain task leaves the chain cacheable, so caching keeps working for the
// pipelines that never touch these features.
func TestFoldStepUnskippableLeavesPlainTasksCacheable(t *testing.T) {
	t.Parallel()

	unskippable, err := foldStepUnskippable(t.Context(), &config.Config{}, config.Step{Task: "t", Run: "true"}, false)
	if err != nil {
		t.Fatal(err)
	}

	if unskippable {
		t.Error("a plain task must not make its chain unskippable")
	}
}
