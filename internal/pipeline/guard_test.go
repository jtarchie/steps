package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// guardTestBuild returns a real build workspace plus the artifact-store
// directory of a "facts" artifact, so a guard command exercised against a
// step declaring `inputs: [facts]` sees actual files.
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

	dir, err := bw.ResourceDir(context.Background(), "facts")
	if err != nil {
		t.Fatal(err)
	}

	return bw, dir
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
		{name: "grep match runs", when: &config.WhenSpec{Run: "grep -q high facts/risk.txt"}, want: true},
		{name: "grep no-match skips (a legitimate false, not an error)", when: &config.WhenSpec{Run: "grep -q nope facts/risk.txt"}, want: false},
		{name: "missing file skips rather than erroring", when: &config.WhenSpec{Run: "test -f absent.txt"}, want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// The guard's view is materialized from the step's declared
			// inputs — isolation is always on, so an undeclared artifact is
			// simply absent.
			step := config.Step{Task: "t", Run: "true", Inputs: config.Inputs("facts"), When: tc.when}

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

// TestEvaluateStepGuardReadsAnUnproducedInputAsAbsent covers the input an
// earlier guarded step never wrote: the guard must get to run and answer
// false, rather than failing on the missing directory before it runs. The
// step's own staging is unchanged — only the guard's view tolerates this.
func TestEvaluateStepGuardReadsAnUnproducedInputAsAbsent(t *testing.T) {
	t.Parallel()

	bw, _ := guardTestBuild(t)

	// "answer" is never produced in this build; "facts" is.
	step := config.Step{
		Task: "t", Run: "true",
		Inputs: config.Inputs("facts", "answer"),
		When:   &config.WhenSpec{Run: "test -s answer/reply.md"},
	}

	got, err := evaluateStepGuard(context.Background(), &config.Config{}, step, bw)
	if err != nil {
		t.Fatalf("an input the build never produced is a false guard, not an error: %v", err)
	}

	if got {
		t.Error("guard should be false when the input it tests was never produced")
	}
}

// TestEvaluateStepGuardSeesTheViewTheStepGets covers the other half of the
// leniency: an input that IS produced must reach the guard, whatever spelling
// the step used to declare it. Reading the step's own inputs: list is not that
// view — a task can inherit its inputs from the tasks: entry it references,
// and input_mapping renames a declared input onto the plan artifact it draws
// from. Both used to leave the guard staring at an empty directory and
// answering a permanent, silent false.
func TestEvaluateStepGuardSeesTheViewTheStepGets(t *testing.T) {
	t.Parallel()

	bw, dir := guardTestBuild(t)

	err := os.WriteFile(filepath.Join(dir, "risk.txt"), []byte("high\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Tasks: []config.Task{
		{Name: "audit", Run: "true", Inputs: config.Inputs("facts")},
	}}

	cases := []struct {
		name string
		step config.Step
	}{
		{
			name: "inputs inherited from the tasks: entry",
			step: config.Step{Task: "audit", When: &config.WhenSpec{Run: "grep -q high facts/risk.txt"}},
		},
		{
			name: "input_mapping names the artifact the store holds",
			step: config.Step{
				Task: "t", Run: "true",
				Inputs:       config.Inputs("evidence"),
				InputMapping: map[string]string{"evidence": "facts"},
				When:         &config.WhenSpec{Run: "grep -q high evidence/risk.txt"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := evaluateStepGuard(context.Background(), cfg, tc.step, bw)
			if err != nil {
				t.Fatalf("evaluateStepGuard: %v", err)
			}

			if !got {
				t.Error("guard read its input as absent, but the step's own space would have materialized it")
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

			spec, _, err := resolveStepRuntime(cfg, tc.step)
			if err != nil {
				t.Fatalf("resolveStepRuntime: %v", err)
			}

			if spec.Image != tc.want {
				t.Errorf("image = %q, want %q", spec.Image, tc.want)
			}
		})
	}
}
