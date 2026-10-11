package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestOwedCallsOnlyIsTheCallsNudgeAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	expect := newStepExpectation(&config.Assert{ToolCalls: wants("post_review"), Nudge: true}, dir, dir)

	_, message := expect.owed(nil)
	if want := assertToolCallsNudge([]string{"post_review"}, false); message != want {
		t.Errorf("message = %q; want only the tool_calls nudge %q", message, want)
	}
}
