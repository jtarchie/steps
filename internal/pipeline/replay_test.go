package pipeline

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestReplayIndexNamesTheStepsItCouldStartFrom: a --from that matches nothing lists what the plan does have, so the typo is fixable without opening the YAML.
func TestReplayIndexNamesTheStepsItCouldStartFrom(t *testing.T) {
	t.Parallel()

	job := &config.Job{Name: "build", Plan: []config.Step{{Task: "compile"}, {Task: "verify"}}}

	_, err := replayIndex(job, "compil")
	if err == nil || !strings.Contains(err.Error(), "(it has: compile, verify)") {
		t.Errorf("replayIndex = %v, want a refusal listing compile, verify", err)
	}
}
