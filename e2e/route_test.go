package e2e

import (
	"os"
	"strings"
	"testing"
)

// TestRunSaysAnInheritedFixMakesTheChainUncacheable: a task step referencing a
// top-level tasks: entry whose fix: is set only there (not inline on the step)
// must be treated as unskippable the same way merkle.PlanChains treats it at
// plan time — route.go's runtime stepForcesUnskippable used to check only the
// step's own literal Fix field, missing a fix: inherited via
// Config.ResolveTask.
//
// Asserted on the note the run prints rather than on the absence of a job_runs
// row, because the row the bug wrote is INERT: the plan-time Unskippable flag
// keeps the chain from ever being asked about, so nothing downstream can tell
// the two apart. The note is the same runtime answer, and it is the half a
// user actually sees.
func TestRunSaysAnInheritedFixMakesTheChainUncacheable(t *testing.T) {
	dir := t.TempDir()
	path := pipelinePath(t, dir)

	pipeline := `
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: http://127.0.0.1:0/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

tasks:
- name: unit
  run: "true"
  fix: fixer

jobs:
- name: build
  plan:
  - task: unit
    inputs: []
`

	err := os.WriteFile(path, []byte(pipeline), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	out := captureStdout(t, func() { mustRun(t, path) })

	const want = "makes this chain uncacheable (fix: agent)"
	if !strings.Contains(out, want) {
		t.Errorf("run output does not contain %q, so a fix: inherited from a tasks: entry "+
			"was not seen at runtime and the chain was recorded as a reusable success:\n%s", want, out)
	}
}

// TestRouteNextOffTheEndOfThePlanDoesNotPanic drives the whole runner over the
// shape the unit tests could not reach: `to: { failure: next }` on the LAST
// step of a plan.
//
// The route resolves one past the end — where an unrouted final step goes
// anyway — and the two places that then wanted to look up "the step routed to"
// both indexed the slice. Both were a hard panic, taking the process down on a
// pipeline whose last outcome says nothing more than "carry on". The route also
// has to CONSUME the failure, or a job that said to keep going still exits red.
func TestRouteNextOffTheEndOfThePlanDoesNotPanic(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: j
  plan:
  - task: probe
    inputs: []
    run: exit 1
    to:
      failure: next
`)

	mustRun(t, path)
}
