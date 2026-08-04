package config

import "testing"

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
