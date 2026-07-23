package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/workspace"
)

// hookTestScope builds a hookScope backed by a real shared build workspace and
// a config whose named tasks each append a marker line to markerFile, so a
// test can assert which hooks fired and in what order.
func hookTestScope(t *testing.T, markerFile string) hookScope {
	t.Helper()

	provider, err := workspace.NewProvider(nil)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { workspace.CloseBuild(bw, "test") })

	cfg := &config.Config{
		Tasks: []config.Task{
			{Name: "on_success", Run: "echo on_success >> " + markerFile},
			{Name: "on_failure", Run: "echo on_failure >> " + markerFile},
			{Name: "on_error", Run: "echo on_error >> " + markerFile},
			{Name: "on_abort", Run: "echo on_abort >> " + markerFile},
			{Name: "ensure", Run: "echo ensure >> " + markerFile},
		},
	}

	return hookScope{cfg: cfg, jobName: "test", label: "step 0", bw: bw}
}

func readMarkers(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp file
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		t.Fatal(err)
	}

	return strings.Fields(strings.TrimSpace(string(data)))
}

// allHooks wires every task hook by its own name, so a marker line names which
// hook ran.
func allHooks() config.Hooks {
	return config.Hooks{
		OnSuccess: &config.Step{Task: "on_success"},
		OnFailure: &config.Step{Task: "on_failure"},
		OnError:   &config.Step{Task: "on_error"},
		OnAbort:   &config.Step{Task: "on_abort"},
		Ensure:    &config.Step{Task: "ensure"},
	}
}

// TestConformance note: verifies on_success/on_failure/on_error firing
// conditions against Concourse's own doc (concourse-ci.org/docs/steps/):
// on_success on a nil error, on_failure on a task-level Failure (outcome.Fail
// — Concourse: "the parent step fails" but "does not recover the failure"),
// on_error on any other error (Concourse: "terminates abnormally in any way
// other than those handled by on_abort or on_failure" — matches
// outcome.Errored's "infrastructure error" bucket exactly). See
// docs/conformance.md.
func TestRunHooksRouting(t *testing.T) {
	tests := []struct {
		name    string
		baseErr error
		want    []string
	}{
		{"success runs on_success then ensure", nil, []string{"on_success", "ensure"}},
		{"marked failure runs on_failure then ensure", outcome.Fail(errors.New("exit 1")), []string{"on_failure", "ensure"}},
		{"plain error runs on_error then ensure", errors.New("infra down"), []string{"on_error", "ensure"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "markers.txt")
			scope := hookTestScope(t, marker)

			err := runHooks(context.Background(), scope, allHooks(), tt.baseErr)

			// A non-nil base error must propagate unchanged (observer semantics).
			if tt.baseErr != nil && err == nil {
				t.Error("runHooks consumed the base error; want it propagated")
			}

			if tt.baseErr == nil && err != nil {
				t.Errorf("runHooks returned %v on a green outcome", err)
			}

			got := readMarkers(t, marker)
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("hooks fired = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunHooksAbortGracePeriod verifies on_abort and ensure both run to
// completion even when the job context is already canceled — they run detached
// under the grace period.
//
// TestConformance note: verifies on_abort fires on a canceled context and
// ensure fires "regardless of whether the parent step succeeds, fails, or
// errors... also executed if the build was aborted," per
// concourse-ci.org/docs/steps/. See docs/conformance.md.
func TestRunHooksAbortGracePeriod(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "markers.txt")
	scope := hookTestScope(t, marker)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = runHooks(ctx, scope, allHooks(), errors.New("interrupted"))

	got := readMarkers(t, marker)
	want := []string{"on_abort", "ensure"}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hooks fired under canceled ctx = %v, want %v", got, want)
	}
}

// TestRunHooksEnsureFailsGreenOutcome verifies a failing ensure hook turns a
// green outcome into a failure, while a failing ensure on an already-failing
// outcome is swallowed.
func TestRunHooksEnsureFailsGreenOutcome(t *testing.T) {
	cfg := &config.Config{Tasks: []config.Task{{Name: "boom", Run: "exit 1"}}}

	provider, err := workspace.NewProvider(nil)
	if err != nil {
		t.Fatal(err)
	}

	newScope := func() hookScope {
		bw, err := provider.NewBuild(context.Background(), "test")
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(func() { workspace.CloseBuild(bw, "test") })

		return hookScope{cfg: cfg, jobName: "test", label: "step 0", bw: bw}
	}

	hooks := config.Hooks{Ensure: &config.Step{Task: "boom"}}

	// Green base: a failing ensure makes it fail.
	greenErr := runHooks(context.Background(), newScope(), hooks, nil)
	if greenErr == nil {
		t.Error("a failing ensure hook did not fail a green outcome")
	}

	// Failing base: a failing ensure is swallowed, base error returned.
	base := outcome.Fail(errors.New("original"))

	failErr := runHooks(context.Background(), newScope(), hooks, base)
	if !errors.Is(failErr, base) {
		t.Errorf("runHooks returned %v, want the original base error", failErr)
	}
}
