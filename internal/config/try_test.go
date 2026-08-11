package config

import (
	"slices"
	"testing"
)

func TestConfigValidateTrySteps(t *testing.T) {
	t.Parallel()

	t.Run("try wrapping a task is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: build
      run: echo hi
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("try wrapping a put is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    out: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - try:
      put: thing
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("try wrapping an agent is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
agents:
- name: reviewer
  source:
    model: lmstudio/qwen2.5-coder
jobs:
- name: build
  plan:
  - try:
      agent: reviewer
      prompt: hello
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("bare try is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try: {}
`)
		wantLoadError(t, path, "try: wraps an unrecognized or empty step")
	})

	t.Run("try wrapping get is rejected", func(t *testing.T) {
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
  - try:
      get: thing
`)
		wantLoadError(t, path, "try: cannot wrap a get step")
	})

	t.Run("try plus another kind is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: build
      run: echo hi
    task: build
`)
		wantLoadError(t, path, "try: is a wrapper")
	})

	t.Run("try wrapping try is accepted (nested)", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      try:
        task: build
        run: echo hi
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("try as a hook is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - task: build
    run: echo hi
    on_failure:
      try:
        task: notify
        run: echo failed
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("try in a hook wrapping get is rejected", func(t *testing.T) {
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
  - task: build
    run: echo hi
    on_failure:
      try:
        get: thing
`)
		wantLoadError(t, path, "try: cannot wrap a get step")
	})
}

// TestTryFieldPlacement pins the division of fields between a try: wrapper and
// the step it wraps. Every case here loaded clean before and then did nothing
// at run time: only the wrapper has a position in the plan, so routing written
// one level deeper never fired, and an assert anywhere inside a try: reported
// its mismatch into a run that continued anyway.
func TestTryFieldPlacement(t *testing.T) {
	t.Parallel()

	t.Run("to: on the wrapped step is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: flaky
      run: exit 1
      to: {failure: recover}
  - task: recover
    run: echo recovered
`)
		wantLoadError(t, path, "to: belongs on the try: step")
	})

	t.Run("max_visits on the wrapped step is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: flaky
      run: exit 1
      max_visits: 2
`)
		wantLoadError(t, path, "max_visits belongs on the try: step")
	})

	t.Run("to: on the wrapper is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: flaky
      run: exit 1
    to: {failure: recover}
  - task: recover
    run: echo recovered
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("assert on the wrapper is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: build
      run: echo hi
    assert: {stdout: hi}
`)
		wantLoadError(t, path, "assert is not valid on a try: step or the step it wraps")
	})

	t.Run("assert on the wrapped step is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
jobs:
- name: build
  plan:
  - try:
      task: build
      run: echo hi
      assert: {stdout: hi}
`)
		wantLoadError(t, path, "assert is not valid on a try: step or the step it wraps")
	})
}

// TestTryIsTransparentToAgentFields covers the other half of the division:
// verdicts:/handoff: stay on the agent step, since that is what internal/agent
// reads. Both used to be accepted-and-ignored or rejected outright, which left
// no working way to combine try: with either.
func TestTryIsTransparentToAgentFields(t *testing.T) {
	t.Parallel()

	const agents = `
agents:
- name: reviewer
  source:
    model: lmstudio/qwen2.5-coder
`

	t.Run("verdicts on the wrapped agent route from inside the wrapper", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, agents+`
jobs:
- name: build
  plan:
  - try:
      agent: reviewer
      prompt: judge it
      verdicts:
        - pass: ship
        - fail: reviewer
    max_visits: 2
  - task: ship
    run: echo shipped
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("a wrapped verdict target is still resolved against the segment", func(t *testing.T) {
		t.Parallel()

		// validateSegment's "does this segment use routing at all" gate reads
		// the targets off the plan step. Without looking through the wrapper
		// there, a wrapped agent's routes skipped validation entirely and
		// reached run time aimed at a step that does not exist.
		path := writeConfig(t, agents+`
jobs:
- name: build
  plan:
  - try:
      agent: reviewer
      prompt: judge it
      verdicts:
        - pass: nowhere
`)
		wantLoadError(t, path, "not a step in the same segment")
	})

	t.Run("wrapped verdicts with no targets classify and carry on", func(t *testing.T) {
		t.Parallel()

		// The routing-free spelling: the tolerated agent still emits a verdict
		// (and still gets the required tool), the plan just continues.
		path := writeConfig(t, agents+`
jobs:
- name: build
  plan:
  - try:
      agent: reviewer
      prompt: judge it
      verdicts: [pass, fail]
`)

		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		if got := cfg.Jobs[0].Plan[0].VerdictNames(); len(got) != 2 {
			t.Errorf("VerdictNames() = %v, want the wrapped agent's two verdicts", got)
		}
	})

	t.Run("handoff on the wrapped agent is accepted", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, agents+`
jobs:
- name: build
  plan:
  - task: judge
    run: "true"
    to: {failure: reviewer, success: done}
  - try:
      agent: reviewer
      prompt: fix it
      handoff: {context: true}
  - task: done
    run: echo done
`)
		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("handoff on the wrapper is rejected", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, agents+`
jobs:
- name: build
  plan:
  - task: judge
    run: "true"
    to: {failure: reviewer, success: done}
  - try:
      agent: reviewer
      prompt: fix it
    handoff: {context: true}
  - task: done
    run: echo done
`)
		wantLoadError(t, path, "handoff is only valid on agent steps")
	})

	t.Run("handoff_note is wired onto the wrapped agent", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, `
agents:
- name: reviewer
  source:
    model: lmstudio/qwen2.5-coder
- name: planner
  source:
    model: lmstudio/qwen2.5-coder
jobs:
- name: build
  plan:
  - agent: planner
    prompt: plan it
    handoff: {note: true}
  - try:
      agent: reviewer
      prompt: review it
`)
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		// The runtime hands internal/agent the WRAPPED step, so that is where
		// the computed receiver has to land.
		got := cfg.Jobs[0].Plan[1].Try.HandoffNoteFrom
		if !slices.Equal(got, []string{"planner"}) {
			t.Errorf("wrapped agent HandoffNoteFrom = %v, want [planner]", got)
		}
	})
}

func TestStepUnwrap(t *testing.T) {
	t.Parallel()

	plain := Step{Task: "build"}
	if plain.Unwrap().Task != "build" {
		t.Error("Unwrap of a non-try step should return the step itself")
	}

	nested := Step{Try: &Step{Try: &Step{Agent: "reviewer"}}}
	if nested.Unwrap().Agent != "reviewer" {
		t.Errorf("Unwrap of a doubled try: = %+v, want the innermost agent step", nested.Unwrap())
	}
}

func TestTryStepKindFieldsSet(t *testing.T) {
	t.Parallel()

	t.Run("try field counted in kindFieldsSet", func(t *testing.T) {
		s := Step{Try: &Step{Task: "build"}}
		fields := s.kindFieldsSet()
		if len(fields) != 1 || fields[0] != "try" {
			t.Errorf("kindFieldsSet = %v, want [try]", fields)
		}
	})

	t.Run("try plus task counted as two", func(t *testing.T) {
		s := Step{Try: &Step{Task: "build"}, Task: "build"}
		fields := s.kindFieldsSet()
		if len(fields) != 2 {
			t.Errorf("kindFieldsSet = %v, want 2 fields", fields)
		}
	})
}
