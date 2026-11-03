package runview

import (
	"testing"

	"github.com/jtarchie/steps/internal/events"
)

func TestStepStatusClassification(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status                string
		open, failed, skipped bool
	}{
		{events.StatusSucceeded, false, false, false},
		{events.StatusFailed, false, true, false},
		{events.StatusErrored, false, true, false},
		{events.StatusAborted, false, true, false},
		{events.StatusSkipped, false, false, true},
		{"", true, false, false},
		{"running", true, false, false},
	}
	for _, tc := range cases {
		step := Step{Status: tc.status}
		if step.open() != tc.open || step.Failed() != tc.failed || step.Skipped() != tc.skipped {
			t.Errorf("%q: open=%v failed=%v skipped=%v, want %v %v %v", tc.status, step.open(), step.Failed(), step.Skipped(), tc.open, tc.failed, tc.skipped)
		}
	}
}
