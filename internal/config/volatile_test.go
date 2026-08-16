package config

import (
	"path/filepath"
	"testing"
)

func TestVolatileRejectedOnAPutStep(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
resource_types:
- name: dummy
  config:
    check: "true"
    in: "true"
    out: "true"

resources:
- name: results
  type: dummy
  source: {}

jobs:
- name: publish
  plan:
  - put: results
    volatile: true
`)

	wantLoadError(t, path, "volatile is only valid on task and agent steps")
}

// TestVolatileRejectedInsideAContainer proves the check walks every step, not
// only the plan's top level — a nested step carrying an inert volatile: is the
// one nobody would notice.
func TestVolatileRejectedInsideAContainer(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
resource_types:
- name: dummy
  config:
    check: "true"
    in: "true"
    out: "true"

resources:
- name: results
  type: dummy
  source: {}

jobs:
- name: publish
  plan:
  - do:
    - put: results
      volatile: true
`)

	wantLoadError(t, path, "volatile is only valid on task and agent steps")
}

// TestVolatileRejectedOnAHook: a hook runs outside the plan's ordering and is
// never looked up in the step cache, so volatile: there would do nothing.
func TestVolatileRejectedOnAHook(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
jobs:
- name: build
  plan:
  - task: work
    run: make
    on_success:
      task: notify
      run: echo done
      volatile: true
`)

	wantLoadError(t, path, "volatile is not valid on hook steps")
}

// TestVolatileAcceptedOnTaskAndAgentSteps is the positive case the rejections
// above are carving out of.
func TestVolatileAcceptedOnTaskAndAgentSteps(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pipeline.yml")
	writeFile(t, path, `
agents:
- name: reviewer
  source:
    endpoint: http://127.0.0.1:1/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: build
  plan:
  - task: work
    volatile: true
    run: date
  - agent: reviewer
    volatile: true
    prompt: review
`)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
}
