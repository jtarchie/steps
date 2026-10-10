package pipeline

import (
	"errors"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// TestTaskFailureOutputShowsTheFixerEachStreamItHas: the fix agent is told whatever the run printed, stream by stream, and an empty stream is left out rather than shown as an empty heading.
func TestTaskFailureOutputShowsTheFixerEachStreamItHas(t *testing.T) {
	t.Parallel()

	reason := errors.New("exit status 2")

	for name, c := range map[string]struct {
		stdout, stderr string
		want           string
	}{
		"stdout only": {"built 3", "", "failure: exit status 2\nexit code: 2\nstdout:\nbuilt 3\n"},
		"stderr only": {"", "missing file", "failure: exit status 2\nexit code: 2\nstderr:\nmissing file\n"},
	} {
		if got := taskFailureOutput(reason, c.stdout, c.stderr, 2); got != c.want {
			t.Errorf("%s: fixer prompt %q, want %q", name, got, c.want)
		}
	}
}

// The budget a re-placement may not exceed is timeout: once per attempt the author granted, so an evicted step cannot quietly run longer than its config promised.
func TestStepBudgetIsTimeoutTimesAttempts(t *testing.T) {
	t.Parallel()

	three := 3

	for name, c := range map[string]struct {
		step    config.Step
		timeout string
		want    time.Duration
	}{
		"one attempt":    {config.Step{}, "10s", 10 * time.Second},
		"three attempts": {config.Step{Attempts: &three}, "10s", 30 * time.Second},
		"no timeout":     {config.Step{Attempts: &three}, "", 0},
	} {
		got, err := stepBudget(c.step, c.timeout)
		if err != nil || got != c.want {
			t.Errorf("%s: stepBudget = %v, %v; want %v", name, got, err, c.want)
		}
	}

	_, err := stepBudget(config.Step{}, "soon")
	if err == nil {
		t.Error("an unparseable timeout produced a budget instead of an error")
	}
}
