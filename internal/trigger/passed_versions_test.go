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

// TestPassedSurvivesAQuietCursorCheck covers the interaction that made every
// `passed:` gate stop working once checks became cursor-driven.
//
// A `passed:` constraint is evaluated against a resource's CURRENT version on
// every poll — necessarily, because the poll that first sees a version comes
// before the upstream job goes green on it, so the gate has to be re-asked
// later. That re-asking needs the resource to be in the poll's observations.
//
// A cursor-driven check asks only for what it has not seen, so it answers
// with nothing almost every poll. Treating "reported nothing" as "has no
// version" dropped the resource out of the observations entirely, and the
// downstream job was then held back forever — silently, since nothing
// distinguishes "waiting on upstream" from "never asked".
//
// Before the cursor a check re-reported its whole window every time, so this
// could not arise; it took a cursor-driven pipeline to expose it.
func TestPassedSurvivesAQuietCursorCheck(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	deployed := filepath.Join(dir, "deployed.txt")

	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' `+feed+`
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: repo
  type: feed
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
    passed: [test]
  - task: ship
    inputs: [repo]
    run: cat repo/n.txt >> `+deployed+`
`)

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	writeVersions(t, feed, "1\n")

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// Item 2 arrives and test is triggered for it.
	writeVersions(t, feed, "1\n2\n")

	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// test goes green on item 2, which is what deploy is waiting for.
	encoded, err := json.Marshal(map[string]any{"n": "2"})
	if err != nil {
		t.Fatal(err)
	}

	err = st.RecordPassedVersion(ctx, "test", "repo", string(encoded), "build-1")
	if err != nil {
		t.Fatal(err)
	}

	// Quiet polls: the check has nothing new to report, which is where the
	// gate used to stop being asked at all.
	_, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (quiet): %v", err)
	}

	drainQueue(ctx, t, cfg, st)

	data, err := os.ReadFile(deployed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("deploy was never released for the version test passed: %v", err)
	}

	if got := strings.Fields(string(data)); len(got) != 1 || got[0] != "2" {
		t.Errorf("deployed %v, want [2]", got)
	}
}
