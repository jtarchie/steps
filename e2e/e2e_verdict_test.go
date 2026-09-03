package e2e

// End-to-end coverage for the two halves of verdicts: that carry no routing:
// a classifier whose whole product is the decision, and the assert: that pins
// it. Both are in the root package for the reason every e2e test is — only
// cli.Run spans config → merkle → agent → route → store, and source.endpoint: is
// the sole injection point.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// classifierPipeline is one agent whose verdicts: are ALL bare — it records
// what it decided and the plan carries on — followed by a task that proves the
// carrying on actually happened.
func classifierPipeline(t *testing.T, dir, endpoint, assertBlock string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: triager
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

jobs:
- name: triage
  plan:
  - agent: triager
    inputs: []
    messages:
      - Classify the report.
    verdicts: [bug, feature, question]
%[3]s  - task: after
    inputs: []
    run: echo continued >> %[2]s
`, endpoint, filepath.Join(dir, "after.log"), assertBlock))
}

// TestEndToEndBareVerdictsClassifyAndCarryOn proves the routing-free spelling
// end to end: the model is still FORCED to decide (the verdict tool is
// synthesized and required exactly as it is in routing mode), the choice is
// recorded, and the plan falls through in declaration order rather than
// jumping — which is what makes a classifier expressible without inventing a
// target for every outcome.
func TestEndToEndBareVerdictsClassifyAndCarryOn(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		says("Looks like a defect."),
		callsTool("verdict", map[string]any{"choice": "bug", "note": "stack trace attached"}),
		says("Filed as a bug."),
	)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")
	mustRun(t, classifierPipeline(t, dir, fake.URL, ""))

	// The plan continued past the verdict step: a bare verdict routes nowhere.
	assertLineCount(t, filepath.Join(dir, "after.log"), 1)

	// The verdict tool was still synthesized, on a step with no routing at
	// all — which is the whole feature: a vocabulary used to be undeclarable
	// without a to: map to spend it on. Request 1 is the preflight ping, so
	// the conversation's first turn is request 2.
	conversation := fake.request(2)
	if !contains(conversation.toolNames(), "verdict") {
		t.Fatalf("tools offered = %v, want the synthesized verdict tool", conversation.toolNames())
	}

	// The enum is the declared list, in declaration order — the property the
	// ordered list exists to keep, and the one a to: map could never hold.
	if want := `"enum":["bug","feature","question"]`; !strings.Contains(conversation.Raw, want) {
		t.Errorf("verdict tool schema does not carry %s", want)
	}
}

// TestEndToEndAssertVerdictPinsTheDecision covers both directions of the new
// assert.verdict: a fixture whose classifier decided as expected is green, and
// one that decided otherwise FAILS.
//
// The failing half is the point. A classifier's product is the choice, and
// before this the only assertable trace of it was that the verdict tool had
// been called — so a fixture passed whatever the model chose.
func TestEndToEndAssertVerdictPinsTheDecision(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	const assertBug = "    assert:\n      verdict: bug\n"

	t.Run("matching verdict passes", func(t *testing.T) {
		dir := t.TempDir()
		fake := newFakeLLM(t,
			says("Looks like a defect."),
			callsTool("verdict", map[string]any{"choice": "bug"}),
			says("Filed."),
		)

		mustRun(t, classifierPipeline(t, dir, fake.URL, assertBug))
		assertLineCount(t, filepath.Join(dir, "after.log"), 1)
	})

	t.Run("different verdict fails the step", func(t *testing.T) {
		dir := t.TempDir()
		fake := newFakeLLM(t,
			says("Looks like a request."),
			callsTool("verdict", map[string]any{"choice": "feature"}),
			says("Filed."),
		)

		err := cli.Run([]string{classifierPipeline(t, dir, fake.URL, assertBug)})
		if err == nil {
			t.Fatal("a classifier that emitted the wrong verdict reported success")
		}

		if !strings.Contains(err.Error(), `assert.verdict: want "bug", got "feature"`) {
			t.Errorf("error = %v, want it to name both verdicts", err)
		}

		// The step failed, so the plan stopped there rather than carrying on.
		assertNoFile(t, filepath.Join(dir, "after.log"))
	})
}

func contains(haystack []string, want string) bool {
	for _, got := range haystack {
		if got == want {
			return true
		}
	}

	return false
}
