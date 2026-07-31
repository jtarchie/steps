package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// guardTestBuild returns a real shared build workspace plus its root
// directory, so a guard command can be exercised against actual files.
func guardTestBuild(t *testing.T) (workspace.BuildWorkspace, string) {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "guard-test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { workspace.CloseBuild(bw, "guard-test") })

	space, err := bw.TaskSpace(context.Background(), "probe", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	return bw, space.Dir()
}

func TestEvaluateStepGuard(t *testing.T) {
	t.Parallel()

	bw, dir := guardTestBuild(t)

	err := os.WriteFile(filepath.Join(dir, "risk.txt"), []byte("high\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}

	cases := []struct {
		name string
		when *config.WhenSpec
		want bool
	}{
		{name: "no guard always runs", when: nil, want: true},
		{name: "exit 0 runs the step", when: &config.WhenSpec{Run: "true"}, want: true},
		{name: "exit 1 skips the step", when: &config.WhenSpec{Run: "false"}, want: false},
		{name: "arbitrary nonzero exit skips", when: &config.WhenSpec{Run: "exit 7"}, want: false},
		{name: "grep match runs", when: &config.WhenSpec{Run: "grep -q high risk.txt"}, want: true},
		{name: "grep no-match skips (a legitimate false, not an error)", when: &config.WhenSpec{Run: "grep -q nope risk.txt"}, want: false},
		{name: "missing file skips rather than erroring", when: &config.WhenSpec{Run: "test -f absent.txt"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			step := config.Step{Task: "t", Run: "true", When: tc.when}

			got, err := evaluateStepGuard(context.Background(), cfg, step, bw)
			if err != nil {
				t.Fatalf("evaluateStepGuard: unexpected error %v", err)
			}

			if got != tc.want {
				t.Errorf("guard %q = %v, want %v", tc.when, got, tc.want)
			}
		})
	}
}

// TestEvaluateStepGuardCommandNotFound proves the exit-code contract holds
// even for a command the shell cannot find: `sh -c` reports 127, which is a
// nonzero exit (a false guard), NOT a runner-level failure. Only the runner
// itself failing to start the command at all is an error — see
// TestResolveStepImage for the paths that can produce one.
func TestEvaluateStepGuardCommandNotFound(t *testing.T) {
	t.Parallel()

	bw, _ := guardTestBuild(t)

	step := config.Step{Task: "t", Run: "true", When: &config.WhenSpec{Run: "definitely-not-a-real-command-xyz"}}

	got, err := evaluateStepGuard(context.Background(), &config.Config{}, step, bw)
	if err != nil {
		t.Fatalf("a 127 exit is a false guard, not an error: %v", err)
	}

	if got {
		t.Error("guard should be false when the command exits nonzero")
	}
}

// TestEvaluateStepGuardUnresolvableStep proves a guard on a step whose own
// config cannot be resolved (an unknown tasks: reference) errors rather than
// silently skipping — an unresolvable step is a pipeline bug, not a "false".
func TestEvaluateStepGuardUnresolvableStep(t *testing.T) {
	t.Parallel()

	bw, _ := guardTestBuild(t)

	// task: with no run: resolves against cfg.Tasks, which is empty here.
	step := config.Step{Task: "missing", When: &config.WhenSpec{Run: "true"}}

	_, err := evaluateStepGuard(context.Background(), &config.Config{}, step, bw)
	if err == nil {
		t.Error("expected an error for an unresolvable task reference")
	}
}

func TestResolveStepImage(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Tasks: []config.Task{{Name: "built", Run: "true", Image: "golang:1.26"}},
		Agents: []config.Agent{
			{Name: "a", Source: config.AgentSource{Model: "lmstudio/qwen"}, Image: "python:3.12"},
		},
		ResourceTypes: []config.ResourceType{{Name: "rt", Image: "alpine/git"}},
		Resources:     []config.Resource{{Name: "r", Type: "rt"}},
	}

	cases := []struct {
		name string
		step config.Step
		want string
	}{
		{name: "inline task, no image", step: config.Step{Task: "t", Run: "true"}, want: ""},
		{name: "task inherits the tasks: entry image", step: config.Step{Task: "built"}, want: "golang:1.26"},
		{name: "step image overrides the task's", step: config.Step{Task: "built", Image: "alpine"}, want: "alpine"},
		{name: "agent image", step: config.Step{Agent: "a", Prompt: "x"}, want: "python:3.12"},
		{name: "put takes its resource type's image", step: config.Step{Put: "r"}, want: "alpine/git"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveStepImage(cfg, tc.step)
			if err != nil {
				t.Fatalf("resolveStepImage: %v", err)
			}

			if got != tc.want {
				t.Errorf("image = %q, want %q", got, tc.want)
			}
		})
	}
}
