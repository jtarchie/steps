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

// TestFailsFastOnlyWhenAsked: an in_parallel: that says nothing, or says false, lets its siblings finish.
func TestFailsFastOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	yes, no := true, false

	for name, tc := range map[string]struct {
		block *InParallel
		want  bool
	}{
		"no block": {nil, false},
		"unset":    {&InParallel{}, false},
		"false":    {&InParallel{FailFast: &no}, false},
		"true":     {&InParallel{FailFast: &yes}, true},
	} {
		if got := tc.block.FailsFast(); got != tc.want {
			t.Errorf("%s: FailsFast = %v, want %v", name, got, tc.want)
		}
	}
}

// Concourse takes in_parallel: as a bare list of steps as well as as
// {steps, limit, fail_fast}; the list form is shorthand for the mapping with
// only steps set, wherever a step can stand.
func TestInParallelTakesABareList(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - in_parallel:
    - task: a
      run: "true"
    - do:
      - in_parallel:
        - task: b
          run: "true"
  on_success:
    in_parallel:
    - task: c
      run: "true"
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	assertBareLists(t, cfg.Jobs[0])
}

func assertBareLists(t *testing.T, job Job) {
	t.Helper()

	top := job.Plan[0].InParallel
	if got := branchNames(top); got != "a,do" || top.Limit != 0 || top.FailFast != nil {
		t.Fatalf("plan in_parallel = %+v, want the two listed steps and nothing else", top)
	}

	if got := branchNames(top.Steps[1].Do[0].InParallel); got != "b" {
		t.Errorf("nested in_parallel branches = %q, want b", got)
	}

	if hook := job.Hooks.OnSuccess; hook == nil || branchNames(hook.InParallel) != "c" {
		t.Errorf("on_success in_parallel = %+v, want task c", hook)
	}

	// The step's line still points at the step, not at the rewritten document.
	if job.Plan[0].Line != 5 {
		t.Errorf("plan step line = %d, want 5", job.Plan[0].Line)
	}
}

// branchNames lists a block's branches by task name, "do" for a do: block.
func branchNames(block *InParallel) string {
	if block == nil {
		return "<nil>"
	}

	names := make([]string, 0, len(block.Steps))

	for _, step := range block.Steps {
		name := step.Task
		if len(step.Do) > 0 {
			name = "do"
		}

		names = append(names, name)
	}

	return strings.Join(names, ",")
}

// The shorthand must not cost a nested step its typo detection: decoding it
// through a custom unmarshaler would have, since KnownFields does not reach
// through one.
func TestInParallelBareListStillRefusesUnknownKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
jobs:
- name: j
  plan:
  - in_parallel:
    - task: a
      rnu: "true"
`)

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "rnu") {
		t.Fatalf("err = %v, want the misspelled key refused", err)
	}

	if !strings.Contains(err.Error(), "line 7") {
		t.Errorf("err = %v, want it to name line 7, where the typo is", err)
	}
}

// Only a step's in_parallel: is shorthand. The same word as a key inside
// free-form params: is the author's data and is left as written.
func TestInParallelShorthandLeavesParamsAlone(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
resource_types:
- name: sink
  config:
    out: "echo '{\"v\": \"1\"}'"
resources:
- name: out
  type: sink
  source: {}
jobs:
- name: j
  plan:
  - put: out
    params:
      in_parallel: [x, y]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	got, ok := cfg.Jobs[0].Plan[0].Params["in_parallel"].([]any)
	if !ok || len(got) != 2 {
		t.Fatalf("params.in_parallel = %#v, want the list as written", cfg.Jobs[0].Plan[0].Params["in_parallel"])
	}
}
