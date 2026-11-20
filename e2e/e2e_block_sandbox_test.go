package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// A block's sandbox: true reaches everything inside it that runs here: a task whose entry never asked, an inline run: and its hook, an agent's shell, a nested block's guard, a load_var's and a put's guard, and the block's own guard. A guard whose touch is refused skips its step, so a missing file is the fence holding.
func TestBlockSandboxFencesItsGuardAndContents(t *testing.T) {
	t.Parallel()

	requireSandbox(t)

	outside := outsideDir(t)

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "touch " + filepath.Join(outside, "from-agent")}),
		says("done"),
	)

	path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: sink
  config:
    check: printf '[]'
    out: |
      printf '{"n": "1"}'

resources:
- name: drop
  type: sink
  source: {}

tasks:
- name: loose
  run: |
    if touch %[1]s/from-entry 2>/dev/null; then exit 11; fi

agents:
- name: writer
  source:
    endpoint: %[2]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: build
  plan:
  - do:
    - task: inline
      outputs: [out]
      run: |
        touch out/inside || exit 10
        if touch %[1]s/from-inline 2>/dev/null; then exit 11; fi
      ensure:
        task: cleanup
        run: |
          if touch %[1]s/from-hook 2>/dev/null; then exit 11; fi
    - task: loose
      inputs: []
    - in_parallel:
      - task: never
        run: exit 12
      when: touch %[1]s/nested-guard
    - load_var: n
      file: out/inside
      inputs: [out]
      when: touch %[1]s/load-var-guard
    - put: drop
      inputs: []
      when: touch %[1]s/put-guard
    - agent: writer
      inputs: []
      messages: ["write it"]
    sandbox: true
  - do:
    - task: skipped
      run: exit 12
    sandbox: true
    when: touch %[1]s/block-guard
`, outside, fake.URL))

	err := stepsRun(t, []string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("block-sandboxed run failed (exit 10: workspace not writable, 11: wrote outside, 12: a guard ran unfenced): %v", err)
	}

	for _, name := range []string{"from-inline", "from-hook", "from-entry", "from-agent", "nested-guard", "load-var-guard", "put-guard", "block-guard"} {
		assertNoFile(t, filepath.Join(outside, name))
	}

	if !fake.request(2).toolResultContains("Operation not permitted") {
		t.Errorf("the model was not shown the denial: %v", fake.request(2).toolResults())
	}
}

// warn judges a block's fence as it does a step's: a sandboxed block's guard and contents are not listed, and the same block without it is.
func TestFenceWarnTrustsABlockSandbox(t *testing.T) {
	t.Parallel()

	requireSandbox(t)

	dir := t.TempDir()
	config := filepath.Join(dir, "steps.yml")
	path := filepath.Join(dir, "app.yml")

	writeConfigFile(t, config, "web:\n  fence: warn\n")
	writePipelineFile(t, path, `
jobs:
- name: build
  plan:
  - do:
    - task: fenced
      run: "true"
    sandbox: true
    when: "true"
  - do:
    - task: loose
      run: "true"
    when: "true"
`)

	stderr := captureStderr(t)

	err := stepsRun(t, []string{"validate", "--syntax-only", "--config", config, path})
	if err != nil {
		t.Fatalf("validate --config: %v", err)
	}

	got := stderr()
	for _, want := range []string{`task "loose"`, `when: guard of job "build" step 1`} {
		if !strings.Contains(got, want) {
			t.Errorf("the unsandboxed block's %s is not listed:\n%s", want, got)
		}
	}

	for _, unwanted := range []string{`task "fenced"`, `when: guard of job "build" step 0`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("the sandboxed block's %s is listed as unfenced:\n%s", unwanted, got)
		}
	}
}
