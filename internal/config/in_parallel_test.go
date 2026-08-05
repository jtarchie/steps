package config

import "testing"

func TestConfigValidateInParallelSteps(t *testing.T) {
	t.Parallel()

	t.Run("in_parallel wrapping tasks is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: lint
        run: echo hi
      - task: test
        run: echo hi
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("empty steps is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps: []
`)
		wantLoadError(t, path, "in_parallel: steps must not be empty")
	})

	t.Run("missing steps is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel: {}
`)
		wantLoadError(t, path, "in_parallel: steps must not be empty")
	})

	t.Run("in_parallel plus another kind is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: lint
        run: echo hi
    task: build
`)
		wantLoadError(t, path, "is a wrapper")
	})

	t.Run("nested in_parallel is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - in_parallel:
          steps:
          - task: a1
            run: echo hi
          - task: a2
            run: echo hi
      - task: b
        run: echo hi
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("in_parallel as a hook is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    on_failure:
      in_parallel:
        steps:
        - task: notify-a
          run: echo a
        - task: notify-b
          run: echo b
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("get steps inside in_parallel are rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - get: thing
      - task: build
        run: echo hi
`)
		wantLoadError(t, path, "get steps are not supported inside in_parallel")
	})
}

func TestInParallelDuplicateOutputs(t *testing.T) {
	t.Parallel()

	t.Run("duplicate outputs across branches rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: one
        run: echo hi
        outputs: [result]
      - task: two
        run: echo hi
        outputs: [result]
`)
		wantLoadError(t, path, "duplicate")
	})

	t.Run("duplicate outputs across nested in_parallel blocks rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - in_parallel:
          steps:
          - task: a
            run: echo hi
            outputs: [result]
      - in_parallel:
          steps:
          - task: b
            run: echo hi
            outputs: [result]
`)
		wantLoadError(t, path, "duplicate")
	})
}

func TestInParallelStepKind(t *testing.T) {
	t.Parallel()

	t.Run("in_parallel kind detected", func(t *testing.T) {
		s := Step{InParallel: &InParallelSpec{Steps: []Step{{Task: "t"}}}}
		kind, ok := s.Kind()
		if !ok {
			t.Fatal("expected Kind() to return ok for in_parallel")
		}
		if kind != StepKindInParallel {
			t.Errorf("Kind() = %q, want %q", kind, StepKindInParallel)
		}
	})

	t.Run("bare in_parallel returns zero kind", func(t *testing.T) {
		s := Step{InParallel: nil}
		_, ok := s.Kind()
		if ok {
			t.Error("expected Kind() to return !ok for nil InParallel")
		}
	})

	t.Run("ambiguous in_parallel+task returns not ok", func(t *testing.T) {
		s := Step{InParallel: &InParallelSpec{Steps: []Step{{Task: "t"}}}, Task: "t"}
		_, ok := s.Kind()
		if ok {
			t.Error("expected Kind() to return !ok for ambiguous in_parallel+task")
		}
	})

	t.Run("kindFieldsSet counts in_parallel", func(t *testing.T) {
		s := Step{InParallel: &InParallelSpec{Steps: []Step{{Task: "t"}}}}
		fields := s.kindFieldsSet()
		if len(fields) != 1 || fields[0] != "in_parallel" {
			t.Errorf("kindFieldsSet = %v, want [in_parallel]", fields)
		}
	})
}

func TestInParallelWithTriggerImageWhen(t *testing.T) {
	t.Parallel()

	t.Run("trigger on in_parallel is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: build
        run: echo hi
    trigger: true
`)
		wantLoadError(t, path, "trigger is only valid on get steps")
	})

	t.Run("image on in_parallel wrapper is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: build
        run: echo hi
    image: alpine
`)
		wantLoadError(t, path, "image is not valid on in_parallel steps")
	})

	t.Run("when on in_parallel wrapper is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: build
        run: echo hi
    when: {run: "true"}
`)
		wantLoadError(t, path, "when is not valid on in_parallel steps")
	})
}

func TestInParallelRouting(t *testing.T) {
	t.Parallel()

	t.Run("within-branch routing is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: first
        run: echo hi
        to: {success: second}
      - task: second
        run: echo hi
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("cross-branch routing is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - in_parallel:
          steps:
          - task: inner-a
            run: echo hi
            to: {success: inner-b}
      - in_parallel:
          steps:
          - task: inner-b
            run: echo hi
`)
		wantLoadError(t, path, "not a step in the same branch")
	})

	t.Run("routing outside block is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: inside
        run: echo hi
        to: {success: outside}
  - task: outside
    run: echo hi
`)
		wantLoadError(t, path, "not a step in the same branch")
	})

	t.Run("backward routing without max_visits is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: first
        run: echo hi
      - task: second
        run: echo hi
        to: {success: first}
`)
		wantLoadError(t, path, "max_visits must be set")
	})

	t.Run("backward routing with max_visits is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - task: first
        run: echo hi
      - task: second
        run: echo hi
        to: {success: first}
        max_visits: 2
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})
}
