package trigger

// A passed:-constrained job builds the version that went green upstream, not
// whatever is newest by the time it runs.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPassedFanOutOnlyBuildsWhatPassed covers a way the gate could fail open.
//
// `passed:` is checked at poll time against ONE version — the newest, and the
// check is "did that exact version go green upstream". A get that also says
// `version: every` then fans out over every version the resource reports,
// which includes older ones nothing has proved anything about. Handing the
// job the resource's whole list would deploy them; handing it the version the
// gate actually cleared does not.
//
// The older version here is not hypothetical: any resource that reports more
// than one version per check has them, and a cold start has a whole backlog.
func TestPassedFanOutOnlyBuildsWhatPassed(t *testing.T) {
	dir := t.TempDir()
	versions := filepath.Join(dir, "versions.json")
	deployed := filepath.Join(dir, "deployed.txt")

	writeVersions(t, versions, `[{"ref":"r0"}]`)

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: listing
  config:
    check: cat `+versions+`
    in: echo {{ .version.ref | shellquote }} > ref.txt
resources:
- name: repo
  type: listing
  source: {}
jobs:
- name: test
  plan:
  - get: repo
    trigger: true
- name: deploy
  plan:
  - get: repo
    trigger: true
    version: every
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/ref.txt >> `+deployed+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Cold start seeds the baseline on r0 and enqueues nothing.
	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// test goes green on r1 and only r1. r0 never passed anything.
	encoded, err := json.Marshal(map[string]any{"ref": "r1"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "build-1")
	if err != nil {
		t.Fatal(err)
	}

	writeVersions(t, versions, `[{"ref":"r0"},{"ref":"r1"}]`)

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy never ran, so the gate proves nothing: %v", err)
	}

	got := strings.Fields(string(data))
	if len(got) != 1 || got[0] != "r1" {
		t.Errorf("deployed %v, want only [r1] — r0 never passed test", got)
	}
}
