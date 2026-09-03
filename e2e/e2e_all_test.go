package e2e

import (
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndEverything is the kitchen-sink ordering fixture, ported from
// pocketci's runtime/backwards/steps/all.yml: one pipeline exercising hooks at
// step, block, wrapper and job level, with the exact interleaved execution
// order pinned in one assert per job — never-fired hooks proven by omission.
//
// Two deliberate departures from the pocketci original, both Concourse-true:
//   - Its abort sections triggered on_abort via `timeout: 1ms` — pocketci's
//     own divergence. Concourse fires on_failure for a timeout (as steps now
//     does), and on_abort only on an external abort, which no pipeline file
//     can spell — here on_abort is pinned by never firing, and its real
//     trigger by TestConformanceAbortFiresOnAbortHook.
//   - Its error sections used a missing binary inside a container; a shell
//     127 classifies as failed here (indistinguishable from the command's own
//     exit), so the errored class comes from the one hermetic infrastructure
//     error available: an unreachable provider endpoint.
func TestEndToEndEverything(t *testing.T) {
	dir := t.TempDir()

	pipeline := `
defaults:
  preflight:
    disabled: true

agents:
- name: unreachable
  source:
    endpoint: http://127.0.0.1:1/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

assert:
  execution: [everything, job-success-hooks, job-failure-hooks, job-error-hooks]

jobs:
- name: everything
  plan:
  - task: task-1
    run: echo hello world
    assert:
      stdout: hello world
      code: 0
  - in_parallel:
      steps:
      - task: parallel-2
        run: "true"
      - task: parallel-3
        run: "true"
  - do:
    - task: do-1
      run: "true"
    - task: do-2
      run: "true"
    on_success:
      task: do-success
      run: "true"
    on_failure:
      task: do-failure-never
      run: "true"
    ensure:
      task: do-ensure
      run: "true"
  - try:
      task: flaky
      run: exit 1
      on_failure:
        task: flaky-failure
        run: "true"
      on_success:
        task: flaky-success-never
        run: "true"
      ensure:
        task: flaky-ensure
        run: "true"
    on_failure:
      task: try-failure
      run: "true"
    ensure:
      task: try-ensure
      run: "true"
  - try:
      agent: unreachable
      messages:
        - Review it.
      attempts: 1
      on_error:
        task: outage-error
        run: "true"
      on_failure:
        task: outage-failure-never
        run: "true"
      ensure:
        task: outage-ensure
        run: "true"
  - try:
      task: slow
      run: sleep 5
      timeout: 1s
      on_failure:
        task: slow-failure
        run: "true"
      on_error:
        task: slow-error-never
        run: "true"
  - task: after
    run: echo made it
  on_success:
    task: job-success
    run: "true"
  on_failure:
    task: job-failure-never
    run: "true"
  on_error:
    task: job-error-never
    run: "true"
  on_abort:
    task: job-abort-never
    run: "true"
  ensure:
    task: job-ensure
    run: "true"
  assert:
    execution:
    - task-1
    - parallel-2
    - parallel-3
    - do-1
    - do-2
    - do-success
    - do-ensure
    - flaky
    - flaky-failure
    - flaky-ensure
    - try-failure
    - try-ensure
    - unreachable
    - outage-error
    - outage-ensure
    - slow
    - slow-failure
    - after
    - job-success
    - job-ensure
    outcome: succeeded

- name: job-success-hooks
  plan:
  - task: works
    run: "true"
  on_success:
    task: succeeded-hook
    run: "true"
  ensure:
    task: ensured
    run: "true"
  assert:
    execution: [works, succeeded-hook, ensured]
    outcome: succeeded

- name: job-failure-hooks
  plan:
  - task: breaks
    run: "false"
  on_failure:
    task: failed-hook
    run: "true"
  ensure:
    task: ensured
    run: "true"
  assert:
    execution: [breaks, failed-hook, ensured]
    outcome: failed

- name: job-error-hooks
  plan:
  - agent: unreachable
    messages:
      - Review it.
    attempts: 1
  on_error:
    task: errored-hook
    run: "true"
  ensure:
    task: ensured
    run: "true"
  assert:
    execution: [unreachable, errored-hook, ensured]
    outcome: failed
`

	path := writePipeline(t, dir, pipeline)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"test", path})
	if err != nil {
		t.Fatalf("steps test failed: %v", err)
	}
}
