package e2e

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// hooksPipeline runs a CLI agent with the given settings line under image:, under image: plus an inherited tag, and on the host.
const hooksPipeline = `
agents:
- name: reviewer
  source:
    model: "@claude/sonnet"
  tools: [read_file]
  %s

jobs:
- name: boxed
  plan:
  - agent: reviewer
    image: alpine
    messages: ["review"]
- name: placed
  tags: [gpu]
  plan:
  - agent: reviewer
    image: alpine
    messages: ["review"]
- name: local
  plan:
  - agent: reviewer
    messages: ["review"]
`

// The CLI process never leaves this machine, so a repo's .claude/ hooks run here whatever image: or tags: promise.
func TestValidateWarnsProjectHooksRunOnThisMachine(t *testing.T) {
	t.Run("project", func(t *testing.T) {
		stderr := captureStderr(t)
		path := writePipeline(t, t.TempDir(), fmt.Sprintf(hooksPipeline, "settings: project"))

		err := cli.Run([]string{"validate", path, "--syntax-only"})
		if err != nil {
			t.Fatalf("a warning failed validate: %v", err)
		}

		var warned []string

		for line := range strings.Lines(stderr()) {
			if strings.HasPrefix(line, "warning: ") && strings.Contains(line, `(agent "reviewer"): settings: project loads the repo's .claude/ hooks, which run on this machine`) {
				warned = append(warned, strings.Fields(line)[2])
			}
		}

		if want := []string{`"boxed"`, `"placed"`}; !slices.Equal(warned, want) {
			t.Errorf("warned for jobs %v, want %v", warned, want)
		}
	})

	t.Run("no settings", func(t *testing.T) {
		stderr := captureStderr(t)
		path := writePipeline(t, t.TempDir(), fmt.Sprintf(hooksPipeline, ""))

		err := cli.Run([]string{"validate", path, "--syntax-only"})
		if err != nil {
			t.Fatalf("validate: %v", err)
		}

		if got := stderr(); strings.Contains(got, "warning:") {
			t.Errorf("a pipeline loading no settings warned:\n%s", got)
		}
	})
}
