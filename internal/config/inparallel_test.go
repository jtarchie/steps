package config

// validateParallelOutputs' branch walk — specifically the container kinds
// branchOutputs must descend into rather than fall through as leaves.

import (
	"strings"
	"testing"
)

// TestParallelOutputsSeesEnsembleMembers pins the ensemble: case of
// branchOutputs: members are ordinary agent steps and may declare outputs:,
// so a member's output name duplicated in a sibling branch is the same
// unordered concurrent write any two branches sharing a name are.
func TestParallelOutputsSeesEnsembleMembers(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
workspace:
  strategy: copy
agents:
- name: a
  source: { model: openai/test }
- name: b
  source: { model: openai/test }
- name: judge
  source: { model: openai/test }
jobs:
- name: j
  plan:
  - in_parallel:
      steps:
      - ensemble:
          agents:
          - agent: a
            outputs: [shared]
            messages:
              - p
          - agent: b
            messages:
              - p
          verdicts:
          - ok: done
          - bad: done
          decide: majority
      - task: work
        inputs: []
        outputs: [shared]
        run: "true"
`)

	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("LoadConfig succeeded; an ensemble member's output duplicated in a sibling branch must be rejected")
	}

	if !strings.Contains(err.Error(), `"shared" is produced by more than one branch`) {
		t.Errorf("error = %v, want the duplicate-branch-output rejection naming \"shared\"", err)
	}
}
