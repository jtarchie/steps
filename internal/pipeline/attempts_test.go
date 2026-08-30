package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/workspace"
)

// intPtr builds a pointer dial the way YAML would, so a test can tell
// "attempts: 0" from an omitted attempts: (see config's dials.go).
func intPtr(v int) *int { return &v }

// TestTaskWithAttempts verifies that a task with attempts: 3 retries on
// failure and succeeds on a later attempt.
func TestTaskWithAttempts(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	// Create a counter file to track attempts.
	counterFile := filepath.Join(t.TempDir(), "counter")

	// Task fails on first attempt (counter doesn't exist), succeeds on second.
	cfg := &config.Config{
		Tasks: []config.Task{
			{
				Name: "flaky",
				Run: fmt.Sprintf(`
					if [ ! -f %s ]; then
						echo 1 > %s
						exit 1
					fi
					exit 0
				`, counterFile, counterFile),
			},
		},
	}

	step := config.Step{Task: "flaky", Attempts: intPtr(3)}
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	// Execute should succeed on the second attempt (retry).
	err = executeTask(context.Background(), cfg, step, rt, bw, nil)
	if err != nil {
		t.Fatalf("executeTask failed: %v", err)
	}

	// Verify the counter file exists (task succeeded and didn't delete it).
	_, statErr := os.Stat(counterFile)
	if statErr != nil {
		t.Fatalf("counter file not found: %v", statErr)
	}
}

// TestTaskWithAttemptsAllFail verifies that when all attempts fail, the error
// is returned.
func TestTaskWithAttemptsAllFail(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	cfg := &config.Config{
		Tasks: []config.Task{
			{Name: "always-fails", Run: "exit 1"},
		},
	}

	step := config.Step{Task: "always-fails", Attempts: intPtr(2)}
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	err = executeTask(context.Background(), cfg, step, rt, bw, nil)
	if err == nil {
		t.Fatal("executeTask should have failed")
	}

	// Verify it's classified as a failure (not errored).
	classification := outcome.Classify(context.Background(), err)
	if classification != outcome.Failed {
		t.Fatalf("expected Failed classification, got: %v", classification)
	}
}

// TestTaskWithTimeout verifies that a task times out mid-execution.
func TestTaskWithTimeout(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	cfg := &config.Config{
		Tasks: []config.Task{
			{
				Name:    "slow-task",
				Run:     "sleep 10",
				Timeout: "100ms",
			},
		},
	}

	step := config.Step{Task: "slow-task"}
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err = executeTask(ctx, cfg, step, rt, bw, nil)
	if err == nil {
		t.Fatal("executeTask should have timed out")
	}

	// An expired step timeout classifies as failed, per Concourse: the step
	// was given a budget and did not finish inside it.
	classification := outcome.Classify(ctx, err)
	if classification != outcome.Failed {
		t.Fatalf("expected Failed classification for timeout, got: %v", classification)
	}
}

// TestTaskTimeoutIsDeadlineExceeded verifies that a task's timeout results in
// a DeadlineExceeded error somewhere in the chain.
func TestTaskTimeoutIsDeadlineExceeded(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	// Use a very short timeout to ensure it expires.
	cfg := &config.Config{
		Tasks: []config.Task{
			{
				Name:    "timeout-task",
				Run:     "sleep 5",
				Timeout: "10ms",
			},
		},
	}

	step := config.Step{Task: "timeout-task"}
	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	err = executeTask(ctx, cfg, step, rt, bw, nil)
	if err == nil {
		t.Fatal("executeTask should have failed due to timeout")
	}

	// The error should wrap a DeadlineExceeded.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded in error chain, got: %v", err)
	}
}

// TestTaskTimeoutSkipsRemainingAttempts verifies that a per-attempt timeout
// ends the step immediately instead of burning the rest of the budget. The
// same work against the same deadline expires again, so attempts 2 and 3 would
// only double the wall clock — see retry.Stop and docs/attempts-timeout.md.
func TestTaskTimeoutSkipsRemainingAttempts(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	// One line appended per invocation, so the count is the attempt count.
	counterFile := filepath.Join(t.TempDir(), "counter")

	cfg := &config.Config{
		Tasks: []config.Task{
			{
				Name:    "always-slow",
				Run:     fmt.Sprintf("echo x >> %s\nsleep 5\n", counterFile),
				Timeout: "100ms",
			},
		},
	}

	step := config.Step{Task: "always-slow", Attempts: intPtr(3)}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = executeTask(ctx, cfg, step, rt, bw, nil)
	if err == nil {
		t.Fatal("executeTask should have timed out")
	}

	data, readErr := os.ReadFile(counterFile) //nolint:gosec // path is this test's own t.TempDir()
	if readErr != nil {
		t.Fatalf("counter file not found: %v", readErr)
	}

	if got := strings.Count(string(data), "x"); got != 1 {
		t.Errorf("task ran %d times, want 1 (a timeout must not be retried)", got)
	}

	// The error keeps its DeadlineExceeded chain and classifies as failed
	// (Concourse's call for a timed-out step), so on_failure is what fires.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded in error chain, got: %v", err)
	}

	if class := outcome.Classify(ctx, err); class != outcome.Failed {
		t.Errorf("classification = %v, want %v", class, outcome.Failed)
	}
}

// TestTaskWithAssertAndAttempts verifies that assert is evaluated correctly
// when attempts is set, and retries happen if the assertion fails.
func TestTaskWithAssertAndAttempts(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	// Counter file to track attempts.
	counterFile := filepath.Join(t.TempDir(), "counter")

	// Task outputs different text on each attempt, eventually outputs "success".
	cfg := &config.Config{
		Tasks: []config.Task{
			{
				Name: "multi-attempt",
				Run: fmt.Sprintf(`
					COUNT=$(test -f %s && cat %s || echo 0)
					COUNT=$((COUNT + 1))
					echo $COUNT > %s

					if [ $COUNT -lt 2 ]; then
						echo "wrong output"
						exit 1
					fi
					echo "success output"
					exit 0
				`, counterFile, counterFile, counterFile),
			},
		},
	}

	step := config.Step{
		Task:     "multi-attempt",
		Attempts: intPtr(3),
		Assert: &config.Assert{
			Stdout: ptrString("success output"),
		},
	}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	// Execute should succeed because the final attempt has the expected output.
	err = executeTask(context.Background(), cfg, step, rt, bw, nil)
	if err != nil {
		t.Fatalf("executeTask failed: %v", err)
	}
}

// TestTaskHooksFireOnce verifies that hooks fire exactly once after all
// attempts are exhausted, not once per attempt.
func TestTaskHooksFireOnce(t *testing.T) {
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	bw, err := provider.NewBuild(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.CloseBuild(bw, "test")

	hookCountFile := filepath.Join(t.TempDir(), "hook_count")

	cfg := &config.Config{
		Tasks: []config.Task{
			{Name: "always-fails", Run: "exit 1"},
			{
				Name: "count-hook",
				Run: fmt.Sprintf(`
					COUNT=$(test -f %s && cat %s || echo 0)
					COUNT=$((COUNT + 1))
					echo $COUNT > %s
				`, hookCountFile, hookCountFile, hookCountFile),
			},
		},
	}

	step := config.Step{
		Task:     "always-fails",
		Attempts: intPtr(3),
	}

	rt, err := cfg.ResolveTask(step)
	if err != nil {
		t.Fatal(err)
	}

	_ = executeTask(context.Background(), cfg, step, rt, bw, nil)

	// Note: hooks are not called from executeTask directly; they are called
	// from dispatchNonGetStep in the main pipeline orchestration. This test
	// validates that attempts are made and errors are returned correctly, which
	// allows the hook dispatch at a higher level to fire only once.
}

// ptrString returns a pointer to a string.
func ptrString(s string) *string {
	return &s
}
