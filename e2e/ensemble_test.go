package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// ensemblePipeline builds a three-member ensemble against one scripted
// provider, routing each verdict to a distinguishable task.
func ensemblePipeline(t *testing.T, dir string, endpoints []string, decide, extra string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer-a
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: reviewer-b
  source: {endpoint: %[6]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: reviewer-c
  source: {endpoint: %[7]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: review
  plan:
  - ensemble:
      verdicts:
        - reject: revise
        - approve: publish
      decide: %[2]s
%[5]s      agents:
      - agent: reviewer-a
        inputs: []
        messages:
          - Review it.
      - agent: reviewer-b
        inputs: []
        messages:
          - Review it.
      - agent: reviewer-c
        inputs: []
        messages:
          - Review it.
  - task: revise
    inputs: []
    run: echo revised >> %[3]s
  - task: publish
    inputs: []
    run: echo published >> %[4]s
`, endpoints[0], decide,
		filepath.Join(dir, "revise.log"), filepath.Join(dir, "publish.log"), extra,
		endpoints[1], endpoints[2]))
}

// members starts one scripted provider per member, so each member's
// conversation is deterministic. A single shared fake would serve whichever
// member's request arrived first, which for concurrent members is nothing a
// test can rely on.
func members(t *testing.T, choices ...string) []string {
	t.Helper()

	endpoints := make([]string, 0, len(choices))

	for _, choice := range choices {
		if choice == "" {
			// A member whose model is down.
			endpoints = append(endpoints, newFakeLLM(t,
				failsWith(500), failsWith(500), failsWith(500)).URL)

			continue
		}

		endpoints = append(endpoints, newFakeLLM(t, votes(choice)...).URL)
	}

	return endpoints
}

// votes scripts one member's conversation: a verdict tool call, then a reply.
func votes(choice string) []turn {
	return []turn{
		callsTool("verdict", map[string]any{"choice": choice, "note": "because " + choice}),
		says("done"),
	}
}

// TestEnsembleMajorityRoutesOnTheDecision is the feature in one run: three
// opinions, majority rules, and the pipeline routes on the DECISION rather
// than on any one model's answer.
func TestEnsembleMajorityRoutesOnTheDecision(t *testing.T) {
	dir := t.TempDir()

	// Members run concurrently, so the fake serves whichever asks first —
	// which is exactly why every member's script here votes the same way
	// except one, and the assertion is about the decision, not about who
	// said what.
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, ensemblePipeline(t, dir, members(t, "approve", "approve", "reject"), "majority", ""))

	assertLineCount(t, filepath.Join(dir, "publish.log"), 1)
	assertNoFile(t, filepath.Join(dir, "revise.log"))
}

// TestEnsembleUnanimousFailsOnDisagreement verifies a rule that cannot be
// satisfied says so rather than picking something.
func TestEnsembleUnanimousFailsOnDisagreement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	err := cli.Run([]string{"run", ensemblePipeline(t, dir, members(t, "approve", "approve", "reject"), "unanimous", ""), "--job", "review"})
	if err == nil {
		t.Fatal("decide: unanimous succeeded with a split vote")
	}

	if !strings.Contains(err.Error(), "the members disagreed") {
		t.Errorf("error does not explain the disagreement: %v", err)
	}
}

// TestEnsembleAnyTakesTheFirstDeclaredVerdict pins the ordering contract:
// verdicts: is a precedence list, so `any` is the "one objection is enough"
// shape when the list runs most to least severe.
func TestEnsembleAnyTakesTheFirstDeclaredVerdict(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, ensemblePipeline(t, dir, members(t, "approve", "approve", "reject"), "any", ""))

	// reject is declared first, so a single reject decides it — the majority
	// vote was approve, and `any` deliberately overrides that.
	//
	// Only the revise branch is asserted: routing jumps to a step and then
	// continues in plan order, so publish runs afterwards too. That is
	// ordinary to: behavior, not something ensemble changes.
	assertLineCount(t, filepath.Join(dir, "revise.log"), 1)
}

// TestEnsembleTieIsAnError covers the decision the issue insists must never be
// silent: with no majority, picking the first vote would be an invisible bug.
func TestEnsembleTieIsAnError(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	// Two members, one each way: no majority, and picking one silently would
	// be the invisible bug this rule exists to prevent.
	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer-a
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
- name: reviewer-b
  source: {endpoint: %[2]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: review
  plan:
  - ensemble:
      verdicts: [reject, approve]
      decide: majority
      agents:
      - agent: reviewer-a
        inputs: []
        messages:
          - Review it.
      - agent: reviewer-b
        inputs: []
        messages:
          - Review it.
`, newFakeLLM(t, votes("approve")...).URL, newFakeLLM(t, votes("reject")...).URL))

	err := cli.Run([]string{"run", path, "--job", "review"})
	if err == nil {
		t.Fatal("a tied majority vote silently decided something")
	}

	if !strings.Contains(err.Error(), "no verdict has one") {
		t.Errorf("error does not report the tie: %v", err)
	}
}

// TestEnsembleMemberErrorFailsByDefault covers the rule that a member which
// ERRORS is not a member that voted. Counting a model failure as a vote is how
// a three-agent ensemble silently becomes a two-agent one.
func TestEnsembleMemberErrorFailsByDefault(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	// Two good votes and one member whose model is down.
	err := cli.Run([]string{"run", ensemblePipeline(t, dir, members(t, "approve", "approve", ""), "majority", ""), "--job", "review"})
	if err == nil {
		t.Fatal("the ensemble succeeded with a member that never voted")
	}

	if !strings.Contains(err.Error(), "did not vote") {
		t.Errorf("error does not distinguish a failure from a vote: %v", err)
	}
}

// TestEnsembleMemberErrorsExcludeDecidesAmongTheRest covers the opt-in policy.
func TestEnsembleMemberErrorsExcludeDecidesAmongTheRest(t *testing.T) {
	dir := t.TempDir()

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, ensemblePipeline(t, dir, members(t, "approve", "approve", ""), "majority", "      member_errors: exclude\n"))

	assertLineCount(t, filepath.Join(dir, "publish.log"), 1)
}
