package e2e

import (
	"fmt"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestFailedAgentStepsPublishTheNodeTheyFailedUnder is the agent half of the
// pipeline package's table (TestFailedStepPublishesTheNodeItFailedUnder),
// here because a model is only fakeable here: a failed agent or ensemble
// step's step_finished carries the node it recorded, which is what lets the
// run page mark the step that broke as changed.
func TestFailedAgentStepsPublishTheNodeTheyFailedUnder(t *testing.T) {
	cases := []struct {
		name string
		kind string
		path func(t *testing.T, dir string) string
	}{
		{name: "agent", kind: "agent", path: func(t *testing.T, dir string) string {
			t.Helper()

			return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: {endpoint: %s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}

jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    messages:
      - Review it.
`, newRepeatingFakeLLM(t, failsWith(500)).URL))
		}},
		{name: "ensemble", kind: "ensemble", path: func(t *testing.T, dir string) string {
			t.Helper()

			return ensemblePipeline(t, dir, members(t, "approve", "approve", "reject"), "unanimous", "")
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path(t, t.TempDir())

			err := cli.Run([]string{"run", path, "--job", "review"})
			if err == nil {
				t.Fatalf("the %s step was supposed to fail", tc.kind)
			}

			assertFailedUnderANode(t, path, tc.kind)
		})
	}
}

// assertFailedUnderANode holds the failed kind step of job review's latest
// run to having published a node that recorded a failure.
func assertFailedUnderANode(t *testing.T, path, kind string) {
	t.Helper()

	st := openStoreFor(t, path)
	hash := finishedHash(t, st, kind)

	if hash == "" {
		t.Fatalf("the failed %s published no hash, want its node", kind)
	}

	nodes, err := st.NodesByHash(t.Context(), []string{hash})
	if err != nil {
		t.Fatal(err)
	}

	node, ok := nodes[hash]
	if !ok || node.Status == "succeeded" {
		t.Errorf("published hash %s names node %+v, want the failure it recorded", hash, node)
	}
}

// finishedHash is the hash job review's latest run published on its kind
// step's step_finished.
func finishedHash(t *testing.T, st store.Store, kind string) string {
	t.Helper()

	runs, err := st.ListRuns(t.Context(), "review", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range rows {
		if row.Type == events.TypeStepFinished && row.StepKind == kind {
			return row.Hash
		}
	}

	t.Fatalf("no %s step finished", kind)

	return ""
}
