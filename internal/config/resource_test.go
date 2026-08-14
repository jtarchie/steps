package config

import "testing"

// TestVersionEveryOnlyOnTheFanOutGet pins where `version: every` is allowed.
//
// A plan fans out at exactly one point — its first get — so `every` anywhere
// else silently fetches a single version (the oldest, on every run). Concourse
// has no such limit: there each input has its own cursor
// (atc/scheduler/algorithm's individualResolver calls NextEveryVersion per
// input), and several inputs may be `every` at once. steps models one fan-out
// point, so the honest answer is a load error rather than a field that is
// accepted and ignored.
func TestVersionEveryOnlyOnTheFanOutGet(t *testing.T) {
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

	t.Run("rejected on a later get", func(t *testing.T) {
		t.Parallel()

		path := writeConfig(t, resources+`
jobs:
- name: j
  plan:
  - get: a
  - get: b
    version: every
`)

		wantLoadError(t, path, "only valid on the FIRST get")
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

		wantLoadError(t, path, "only valid on the FIRST get")
	})
}
