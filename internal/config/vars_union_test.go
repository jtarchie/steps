package config

import "testing"

func TestInParallelYieldsEveryBranchCapture(t *testing.T) {
	t.Parallel()

	plan := `
  - in_parallel:
      steps:
      - load_var: a
        file: meta/v
        inputs: [meta]
      - load_var: b
        file: meta/v
        inputs: [meta]
  - task: after
    run: echo ((a)) ((b))
`
	path := writeConfig(t, varsPick+plan)

	_, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("in_parallel branches' captures must all be visible after the block: %v", err)
	}
}

func TestUnionDoesNotMutateReceiver(t *testing.T) {
	t.Parallel()

	base := varSet{"a": true}
	got := base.union(varSet{"b": true})

	if !got["a"] || !got["b"] || len(got) != 2 {
		t.Fatalf("union = %v, want a and b", got)
	}

	if len(base) != 1 {
		t.Fatalf("union mutated its receiver: %v", base)
	}
}

func TestStepLabelNamesJobStepAndLine(t *testing.T) {
	t.Parallel()

	job := Job{Name: "build", Plan: []Step{{}, {Line: 7}}}

	if got, want := job.stepLabel(0), `job "build" step 0`; got != want {
		t.Fatalf("stepLabel(0) = %q, want %q", got, want)
	}

	if got, want := job.stepLabel(1), `job "build" step 1`+job.Plan[1].at(); got != want || job.Plan[1].at() == "" {
		t.Fatalf("stepLabel(1) = %q, want %q (with a line suffix)", got, want)
	}
}

func TestVarErrorCitesStepLabel(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, varsPick+"  - task: after\n    run: echo ((nope))\n")
	wantLoadError(t, path, `job "build" step 1`)
}
