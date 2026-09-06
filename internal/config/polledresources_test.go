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

// TestPolledResourceNamesDedupesAliases confirms two get steps aliasing the
// same resource (via resource:) poll it once, matched by the RESOLVED
// resource name rather than either alias — mirroring
// internal/trigger.TestResourcesAndAffectedJobsResolveGetAlias, which proves
// the same fact one hop away through the delegation. Also exercises
// ResourceIsPolled directly, since internal/pipeline is the only other
// caller and asks it per resolved version rather than per pipeline.
func TestPolledResourceNamesDedupesAliases(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: repo
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: source
    resource: repo
    trigger: true
- name: other
  plan:
  - get: mirror
    resource: repo
    trigger: true
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	got := cfg.PolledResourceNames()
	want := []string{"repo"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("PolledResourceNames = %v, want %v (both aliases resolve to repo, deduped)", got, want)
	}

	if !cfg.ResourceIsPolled("repo") {
		t.Error("ResourceIsPolled(repo) = false, want true — repo is trigger:true under both aliases")
	}

	if cfg.ResourceIsPolled("source") || cfg.ResourceIsPolled("mirror") {
		t.Error("ResourceIsPolled matched an alias name — it must match the RESOLVED resource name only")
	}
}

// TestResourceIsPolledAgreesWithPolledResourceNames checks the two forms
// answer the same question for a plain get (neither trigger: nor passed:) —
// ResourceIsPolled is a faster path to the same membership test, not a
// different rule.
func TestResourceIsPolledAgreesWithPolledResourceNames(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: plain-get
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: plain-get
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.ResourceIsPolled("plain-get") {
		t.Error("ResourceIsPolled(plain-get) = true, want false — a plain get with no trigger:/passed: is not polled")
	}

	if len(cfg.PolledResourceNames()) != 0 {
		t.Errorf("PolledResourceNames = %v, want empty", cfg.PolledResourceNames())
	}
}
