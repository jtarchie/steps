package agent

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// A model trained on harnesses that cap a shell call backgrounds a test suite and polls it with sleep: measured at 26 of 134 run_shell calls in one self-build run.
func TestRunShellTellsTheModelItNeedNotBackgroundAndPoll(t *testing.T) {
	t.Parallel()

	for _, image := range []string{"", "golang:1.27"} {
		desc := builtinAgentTools(image)[config.RunShellBuiltinName].decl.Description
		for _, want := range []string{"no time limit of its own", "in the foreground", "never background a command and poll it with sleep"} {
			if !strings.Contains(desc, want) {
				t.Errorf("image %q: run_shell's description lacks %q:\n%s", image, want, desc)
			}
		}
	}
}
