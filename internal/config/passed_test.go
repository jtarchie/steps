package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestPassedUpstreamMustGetOrPutTheResource: a put is an output of the build
// and counts, except in a job-level hook that only runs when the build is not
// green.
func TestPassedUpstreamMustGetOrPutTheResource(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		upstream string
		refused  bool
	}{
		"get":                {"plan: [{get: repo}]", false},
		"put":                {"plan: [{put: repo}]", false},
		"put in try":         {"plan: [{try: {put: repo}}]", false},
		"put in on_success":  {"plan: [{task: t, run: 'true'}]\n  on_success: {put: repo}", false},
		"put in ensure":      {"plan: [{task: t, run: 'true'}]\n  ensure: {put: repo}", false},
		"put in on_failure":  {"plan: [{task: t, run: 'true'}]\n  on_failure: {put: repo}", true},
		"put in on_error":    {"plan: [{task: t, run: 'true'}]\n  on_error: {put: repo}", true},
		"put in on_abort":    {"plan: [{task: t, run: 'true'}]\n  on_abort: {put: repo}", true},
		"neither":            {"plan: [{task: t, run: 'true'}]", true},
		"put of another one": {"plan: [{put: other}]", true},
	} {
		t.Run(name, func(t *testing.T) {
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
- name: other
  type: dummy
  source: {}
jobs:
- name: up
  `+tc.upstream+`
- name: down
  plan:
  - get: repo
    passed: [up]
`)

			_, err := LoadConfig(path)
			if tc.refused != (err != nil) {
				t.Fatalf("refused = %v, want %v (err %v)", err != nil, tc.refused, err)
			}

			if err != nil && !strings.Contains(err.Error(), "neither gets nor puts resource") {
				t.Errorf("error %q does not say why", err)
			}
		})
	}
}

// TestJobInputsAndOutputsAreWhatTheGraphDraws: every get and put a job can
// run — inside a try:, in a hook — under the RESOURCE's name rather than an
// alias, each resource once.
func TestJobInputsAndOutputsAreWhatTheGraphDraws(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- {name: repo, type: dummy, source: {}}
- {name: docs, type: dummy, source: {}}
- {name: image, type: dummy, source: {}}
- {name: alert, type: dummy, source: {}}
jobs:
- name: up
  plan: [{get: repo}]
- name: build
  plan:
  - get: source
    resource: repo
    trigger: true
    passed: [up]
  - get: repo
  - get: docs
  - try: {put: image}
  - put: shipped
    resource: image
  on_failure: {put: alert}
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	job, err := cfg.FindJob("build")
	if err != nil {
		t.Fatal(err)
	}

	want := []JobInput{
		{Resource: "repo", Trigger: true, Passed: []string{"up"}},
		{Resource: "docs"},
	}
	if got := job.Inputs(); !reflect.DeepEqual(got, want) {
		t.Errorf("Inputs() = %+v, want %+v", got, want)
	}

	if got, want := job.Outputs(), []string{"image", "alert"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Outputs() = %v, want %v", got, want)
	}
}
