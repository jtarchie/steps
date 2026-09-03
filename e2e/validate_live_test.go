package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/pipeline"
)

// preflightPipeline renders a job whose plan runs a task before its agent, so
// a test can prove preflight stopped the run before ANY step — not merely
// before the agent step.
func preflightPipeline(t *testing.T, dir, endpoint, extra string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
%[2]s

jobs:
- name: publish
  plan:
  - task: prepare
    inputs: []
    run: echo ran >> %[3]s
  - agent: writer
    inputs: []
    messages:
      - Write something.
`, endpoint, extra, filepath.Join(dir, "task.log")))
}

// TestPreflightStopsBeforeAnyStepRuns is the whole point of the feature: a
// model that is not serving is discovered in seconds, before the plan spends
// anything, rather than at the moment the agent step is finally reached — which
// for a real plan was half an hour and a chunk of a capped budget in.
func TestPreflightStopsBeforeAnyStepRuns(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()

	outage := make([]turn, 5)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	fake := newFakeLLM(t, outage...)
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("run succeeded against a model that never answers")
	}

	// The message has to say nothing ran, or a reader cannot tell this from
	// an ordinary mid-plan failure — and "nothing ran" is the entire value.
	for _, want := range []string{"preflight failed", "no steps were run", "test-model"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}

	// The task ahead of the agent in the plan never ran either.
	assertNoFile(t, filepath.Join(dir, "task.log"))
}

// TestPreflightPassesThroughToTheRun verifies a live model costs one probe and
// then gets out of the way.
func TestPreflightPassesThroughToTheRun(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"), says("done"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	assertLineCount(t, filepath.Join(dir, "task.log"), 1)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want 2 (one probe, one conversation turn)", got)
	}
}

// TestPreflightCachesAcrossRuns pins the requirement that makes preflight
// usable under `steps web`: without a cache, every poll interval pays for a
// probe request against every model in the pipeline.
func TestPreflightCachesAcrossRuns(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"), says("first"), says("second"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)
	mustRun(t, path)

	// Three requests, not four: one probe shared by both runs, plus one
	// conversation turn each.
	if got := fake.requestCount(); got != 3 {
		t.Errorf("provider requests = %d, want 3 (the second run must trust the cached probe)", got)
	}
}

// TestNoPreflightFlagSkipsTheCheck covers the escape hatch.
func TestNoPreflightFlagSkipsTheCheck(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("done"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"run", path, "--job", "publish", "--no-preflight"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// One request: the conversation turn. No probe was made, which is the
	// only observable difference.
	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1 (--no-preflight must probe nothing)", got)
	}
}

// TestPerAgentPreflightOptOut covers the per-agent escape hatch, which exists
// for a model expected to be slow to WAKE — a cold local model would fail a
// probe that a real conversation would have waited out.
func TestPerAgentPreflightOptOut(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("done"))
	path := preflightPipeline(t, dir, fake.URL, "  preflight: false")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"run", path, "--job", "publish"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1 (an opted-out agent must not be probed)", got)
	}
}

// TestPreflightNamesTheEndpointContrast pins the diagnostic that a human
// reached for by hand: when one model on an endpoint answers and another does
// not, the same account, key, and endpoint are demonstrably fine — so the
// message must say so rather than leaving the reader to suspect credentials.
func TestPreflightNamesTheEndpointContrast(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()

	// The fake answers in script order, and preflight probes agents in plan
	// order: `healthy` first, then `broken`.
	// The broken probe 500s through the default retry budget — a real outage
	// does not recover between backoffs, and the script must outlast it.
	fake := newFakeLLM(t, says("probe ok"),
		failsWith(http.StatusInternalServerError),
		failsWith(http.StatusInternalServerError),
		failsWith(http.StatusInternalServerError))

	path := writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: healthy
  source:
    endpoint: %[1]s/v1/
    model: good-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
- name: broken
  source:
    endpoint: %[1]s/v1/
    model: bad-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: publish
  plan:
  - agent: healthy
    inputs: []
    messages:
      - Plan it.
  - agent: broken
    inputs: []
    messages:
      - Build it.
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"run", path, "--job", "publish"})
	if err == nil {
		t.Fatal("run succeeded with one of its models down")
	}

	if !strings.Contains(err.Error(), "other models on this endpoint responded") {
		t.Errorf("error does not draw the contrast that identifies the model as the problem: %v", err)
	}
}

// TestValidateLiveRunsNothing covers `steps validate --live`: the same probe
// `steps preflight` used to be, asked deliberately, before committing to an
// hour-long run.
//
// A depth on validate rather than a verb of its own, because both are reads
// of the same question — validate answers "is this runnable at all", --live
// answers "is it runnable right now" — and the flag says which without
// changing what the command does.
func TestValidateLiveRunsNothing(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	fake := newFakeLLM(t, says("probe ok"))
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"validate", path, "--live", "--job", "publish"})
	if err != nil {
		t.Fatalf("validate --live failed against a live model: %v", err)
	}

	assertNoFile(t, filepath.Join(dir, "task.log"))
}

// TestValidateLiveReportsAnUnreachableModel is the other half: --live has to
// FAIL on what plain validate cannot see. The file is fine and the machine
// has the credential; only the service is down.
func TestValidateLiveReportsAnUnreachableModel(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()

	outage := make([]turn, 5)
	for i := range outage {
		outage[i] = failsWith(http.StatusInternalServerError)
	}

	fake := newFakeLLM(t, outage...)
	path := preflightPipeline(t, dir, fake.URL, "")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	// Plain validate passes: nothing about a dead endpoint is knowable from
	// the file or from this machine.
	err := cli.Run([]string{"validate", path})
	if err != nil {
		t.Fatalf("validate without --live should not reach the model: %v", err)
	}

	err = cli.Run([]string{"validate", path, "--live"})
	if err == nil {
		t.Fatal("--live passed against a model that answers 500 to everything")
	}

	if !strings.Contains(err.Error(), "cannot run here") {
		t.Errorf("failure does not read as a pipeline that cannot run: %v", err)
	}
}

// TestValidateDepthFlagsRefuseToContradict.
//
// The three depths are ordered — the file, this machine, the services — so
// asking for the shallowest and the deepest together means nothing, and
// --job without --live would be a flag that reads as configured while
// binding nothing.
func TestValidateDepthFlagsRefuseToContradict(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: compile
    inputs: []
    run: "true"
`)

	for name, args := range map[string][]string{
		"live with syntax-only": {"validate", path, "--live", "--syntax-only"},
		"job without live":      {"validate", path, "--job", "build"},
	} {
		t.Run(name, func(t *testing.T) {
			err := cli.Run(args)
			if err == nil {
				t.Fatalf("%v was accepted", args)
			}
		})
	}
}

// TestValidateLiveRefusesWhenPreflightIsDisabled.
//
// --live is the one depth whose whole claim is that something was ASKED. With
// defaults.preflight.disabled: true the probes return no problems having
// contacted nothing, so validate printed "ok … — every model and MCP server
// responded" for a pipeline pointed at an endpoint that is not there: a
// positive claim about a service nobody spoke to, produced from the pipeline's
// own YAML.
func TestValidateLiveRefusesWhenPreflightIsDisabled(t *testing.T) {
	pipeline.ResetPreflightCache()

	dir := t.TempDir()
	// Port 1 is nothing: if a probe were made, it would fail.
	path := preflightPipeline(t, dir, "http://127.0.0.1:1", "\ndefaults:\n  preflight:\n    disabled: true")

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"validate", path, "--live"})
	if err == nil {
		t.Fatal("--live vouched for a pipeline that had turned the probe off")
	}

	if !strings.Contains(err.Error(), "preflight") {
		t.Errorf("refusal does not name what turned the probe off: %v", err)
	}

	// The shallower depths still answer: nothing about the file or this
	// machine depends on the probe.
	err = cli.Run([]string{"validate", path})
	if err != nil {
		t.Errorf("plain validate should still pass: %v", err)
	}
}
