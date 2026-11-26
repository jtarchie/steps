package e2e

import (
	"path/filepath"
	"strings"
	"testing"
)

// The claude CLI files its auto-memory by the git repository its working directory sits in, and --setting-sources "" does not turn that off: a step workspace under an operator's repo was handed that repo's saved notes, which a chat bot then recited.
func TestE2ECLIAgentIsNotHandedTheOperatorsAutoMemory(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(t.TempDir(), "automemory")
	writeFakeClaude(t, "printf '%s' \"${CLAUDE_CODE_DISABLE_AUTO_MEMORY-unset}\" > "+seen+"\necho '"+cliInitEvent(cliPipelineGrantedTools()...)+"'\necho '"+cliResultEvent("looks fine", 2)+"'")

	_ = stepsRun(t, []string{cliPipeline(t, dir)})

	if got := strings.TrimSpace(readFileString(t, seen)); got != "1" {
		t.Errorf("the CLI child saw CLAUDE_CODE_DISABLE_AUTO_MEMORY=%q, want 1", got)
	}
}
