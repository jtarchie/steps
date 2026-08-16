package config

import "testing"

// TestVersionEveryOnlyOnTopLevelGets pins where `version: every` is allowed.
//
// Any top-level plan get may be `every` — each advances its own cursor per
// input set, Concourse's model (atc/scheduler/algorithm's individualResolver
// calls NextEveryVersion per input). A get inside a hook executes within a
// build whose input set is already bound, so `every` there silently fetches
// one version forever — a load error, not a field accepted and ignored. Two
// `every` gets aliasing one resource would share one cursor: also rejected.
func TestVersionEveryOnlyOnTopLevelGets(t *testing.T) {
	t.Parallel()

	const resources = `
resource_types:
- name: dummy
  config:
    check: printf '[{"ref":"v1"}]'
    in: echo v1 > ./ref
resources:
- name: a
  type: dummy
  source: {}
- name: b
  type: dummy
  source: {}
`

	t.Run("allowed on the first get", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
    version: every
  - get: b
`)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("allowed on a later top-level get", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
  - get: b
    version: every
`)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("allowed on several gets at once", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
    version: every
  - get: b
    version: every
`)

		_, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
	})

	t.Run("rejected inside a hook", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
    on_success:
      get: b
      version: every
`)

		wantLoadError(t, path, "only valid on a top-level get")
	})

	t.Run("rejected when two every gets alias one resource", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
    version: every
  - get: also-a
    resource: a
    version: every
`)

		wantLoadError(t, path, "would share one")
	})
}
