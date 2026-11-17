package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A step's own sandbox: true fences an inline run: (which has no entry to ask for it), a job hook, and a step whose tasks: entry never asked; each script exits nonzero when its write escaped.
func TestStepSandboxFencesInlineRunsHooksAndEntries(t *testing.T) {
	t.Parallel()

	requireSandbox(t)

	dir := t.TempDir()
	outside := outsideDir(t)

	path := writePipeline(t, dir, fmt.Sprintf(`
tasks:
- name: loose
  run: |
    if touch %[1]s/from-entry 2>/dev/null; then exit 11; fi

jobs:
- name: build
  plan:
  - task: inline
    sandbox: true
    network: none
    outputs: [out]
    run: |
      touch out/inside || exit 10
      if touch %[1]s/from-inline 2>/dev/null; then exit 11; fi
  - task: loose
    sandbox: true
    inputs: []
  ensure:
    task: cleanup
    sandbox: true
    run: |
      if touch %[1]s/from-hook 2>/dev/null; then exit 11; fi
`, outside))

	err := stepsRun(t, []string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("step-sandboxed run failed (exit 10: workspace not writable, 11: wrote outside): %v", err)
	}

	for _, name := range []string{"from-inline", "from-entry", "from-hook"} {
		assertNoFile(t, filepath.Join(outside, name))
	}
}

// The agent half: an entry that never asked is fenced by the step that runs it, and the model is shown the denial.
func TestStepSandboxFencesAnAgentsShell(t *testing.T) {
	t.Parallel()

	requireSandbox(t)

	target := filepath.Join(outsideDir(t), "agent-escaped")

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "touch " + target}),
		says("done"),
	)

	path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: publish
  plan:
  - agent: writer
    sandbox: true
    inputs: []
    messages: ["write it"]
`, fake.URL))

	mustRun(t, "run", path, "--job", "publish")

	assertNoFile(t, target)

	if !fake.request(2).toolResultContains("Operation not permitted") {
		t.Errorf("the model was not shown the denial: %v", fake.request(2).toolResults())
	}
}

// The step's spelling only strengthens: false cannot unfence an entry, so it is refused rather than read as meaning something; a get, put or block has no command of its own to fence; and a container is already its own fence.
func TestStepSandboxRefusesWhatItCannotMean(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, step, want string }{
		{"false", "  - task: probe\n    sandbox: false\n    inputs: []\n", "sandbox: false"},
		{"get", "  - get: ticks\n    sandbox: true\n", "not valid on get"},
		{"block", "  - do:\n    - task: probe\n      inputs: []\n    sandbox: true\n", "runs none"},
		{"image", "  - task: probe\n    sandbox: true\n    image: alpine:3\n    inputs: []\n", "image:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writePipeline(t, t.TempDir(), `
resource_types:
- name: stamp
  config:
    check: printf '[]'
    in: "true"

resources:
- name: ticks
  type: stamp
  source: {}

tasks:
- name: probe
  run: "true"

jobs:
- name: build
  plan:
`+tc.step)

			err := stepsRun(t, []string{"validate", "--syntax-only", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}
