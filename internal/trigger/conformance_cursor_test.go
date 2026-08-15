package trigger

// The check cursor, across the seam where it actually broke.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestConformanceRunDoesNotDependOnPollerCursor pins the rule that a get
// step's versions must not depend on how far the POLLER has advanced.
//
// The cursor (resource_checks) means "the newest version the poller has
// detected and dispatched", and pollOnce advances it as soon as the affected
// jobs are ENQUEUED — before any of them runs. A get re-derives its versions
// by running check again, so if that re-derivation is handed the cursor it
// asks a different question than the poll did: a check written the way the
// docs prescribe ("everything since the cursor") answers the poll with the
// new versions, and then answers the job the poll just enqueued with
// NOTHING. The job would go green having processed nothing, and while
// resource_versions now remembers what a check reported, a job that has
// silently decided it has no work does not come back for it.
//
// This is deliberately an integration test over pollOnce AND RunJob. The
// defect is invisible to either alone: the poll returns the right versions,
// the run does the right thing with what its check returns, and only the
// order of the two reveals it.
//
// Concourse doc: concourse-ci.org/docs/resource-types/implementing/ — check
// receives the current version. The divergence steps takes, and the reason,
// is recorded in docs/conformance.md.
func TestConformanceRunDoesNotDependOnPollerCursor(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.txt")
	processed := filepath.Join(dir, "processed.txt")

	// A cursor-using check: emit only the items newer than the cursor. This
	// is the shape docs/resources.md prescribes and the shipped Slack
	// pipeline uses (Slack's `oldest`, GitHub's `since`).
	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%%s{\"n\":\"%%s\"}", (k++?",":""), $1} END{printf "]"}' %s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
    trigger: true
    version: every
  - task: work
    inputs: [items]
    run: cat items/n.txt >> %s
`, feed, processed))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	// Cold start seeds the baseline and enqueues nothing.
	writeFeed(t, feed, "1\n")

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// Three new items arrive and the poll enqueues the job for them.
	writeFeed(t, feed, "1\n2\n3\n4\n")

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if len(enqueued) != 1 || enqueued[0] != "build" {
		t.Fatalf("enqueued = %v, want [build]", enqueued)
	}

	// Now the worker drains it, exactly as runWorker does — after the poll
	// has already advanced the cursor.
	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	data, err := os.ReadFile(processed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("the job processed nothing at all and still succeeded: %v", err)
	}

	got := strings.Fields(string(data))
	for _, want := range []string{"2", "3", "4"} {
		if !slicesContains(got, want) {
			t.Errorf("processed %v, want it to include %q — the versions the poll enqueued this job FOR", got, want)
		}
	}
}

func writeFeed(t *testing.T, path, contents string) {
	t.Helper()

	err := os.WriteFile(path, []byte(contents), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func slicesContains(haystack []string, want string) bool {
	for _, item := range haystack {
		if item == want {
			return true
		}
	}

	return false
}
