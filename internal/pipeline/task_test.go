package pipeline

import (
	"errors"
	"testing"
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
