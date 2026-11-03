package trigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test's own deadline is what bounds a regression: without the per-resource limit the hung check never returns.
func TestAHungCheckTimesOutAndTheNextResourceIsStillChecked(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: hung
  config:
    check: touch `+started+` && exec sleep 3600
    in: "true"
- name: fine
  config:
    check: echo '[{"n":"1"}]'
    in: "true"
resources:
- name: stuck
  type: hung
  source: {}
  check_timeout: 100ms
- name: healthy
  type: fine
  source: {}
jobs:
- name: build
  plan:
  - get: stuck
    trigger: true
  - get: healthy
    trigger: true
  - task: work
    run: echo built
`)
	st := mustOpenStore(t, dir)

	_, err := pollOnce(t.Context(), cfg, st)
	if err == nil {
		t.Fatal("a check that outlived check_timeout was not reported as failed")
	}

	_, statErr := os.Stat(started)
	if statErr != nil {
		t.Fatalf("the hung check never started, so the timeout proved nothing: %v", statErr)
	}

	reason := checkErrorFor(t, st, "stuck")
	for _, want := range []string{`"stuck"`, "check_timeout", "100ms"} {
		if !strings.Contains(reason, want) {
			t.Errorf("recorded reason %q does not contain %q", reason, want)
		}
	}

	_, found, lerr := st.LastChecked(t.Context(), "healthy")
	if lerr != nil || !found {
		t.Errorf("the resource after the hung one was not checked: found=%v err=%v", found, lerr)
	}
}
