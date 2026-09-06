package config

import (
	"reflect"
	"testing"
)

// TestPolledResourceNames pins the set internal/trigger's poller checks and
// owns the resource_checks baseline for: any resource a get step references
// with trigger: true or passed:, in first-seen order — everything else (a
// plain get with neither) is excluded, because nothing polls it and the
// pipeline package's per-run refresh owns showing its version instead.
func TestPolledResourceNames(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: triggered
  type: dummy
  source: {}
- name: passed-only
  type: dummy
  source: {}
- name: plain-get
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: triggered
    trigger: true
  - get: plain-get
  - get: passed-only
- name: downstream
  plan:
  - get: passed-only
    passed: [build]
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	got := cfg.PolledResourceNames()
	want := []string{"triggered", "passed-only"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("PolledResourceNames = %v, want %v", got, want)
	}
}
