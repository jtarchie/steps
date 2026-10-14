package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store/sqlite"
)

// independentFeeds is two resources, each with its own job. `down`'s check
// fails while FEED.broken exists — an upstream API that has gone away — and
// its job comes first, so it is the first resource a poll checks.
const independentFeeds = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: up-feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' FEED
    in: echo {{ .version.n | shellquote }} > n.txt
- name: down-feed
  config:
    check: |
      if [ -e FEED.broken ]; then echo "upstream is down" >&2; exit 1; fi
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' FEED.down
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: down
  type: down-feed
  source: {}
- name: items
  type: up-feed
  source: {}
jobs:
- name: build-down
  plan:
  - get: down
    trigger: true
  - task: work
    inputs: [down]
    run: cat down/n.txt >> PROCESSED.down
- name: build
  plan:
  - get: items
    trigger: true
  - task: work
    inputs: [items]
    run: cat items/n.txt >> PROCESSED
`

// TestWatchChecksEachResourceIndependently: one resource whose check fails
// does not stop the others in its pipeline from triggering — Concourse checks
// each resource on its own — and the failed one, never advanced while it was
// failing, builds what it missed once it recovers.
func TestWatchChecksEachResourceIndependently(t *testing.T) {
	fixture := newWatchFixture(t, independentFeeds)
	fixture.resources = []string{"items", "down"}
	broken := fixture.feed + ".broken"
	downFeed := fixture.feed + ".down"
	downProcessed := fixture.processed + ".down"

	fixture.items(t, 1)
	fixture.write(t, downFeed, "1\n")
	fixture.coldStart(t)
	fixture.write(t, downProcessed, "")

	fixture.write(t, broken, "")

	// The daemon is polling every few milliseconds while this runs. A check already under way when broken appeared has passed its test for it, and would go on to read the feed — so version 2 must not exist until a check has failed on broken, after which every check sees it.
	waitForCheckError(t, fixture, "down")

	fixture.write(t, downFeed, "1\n2\n")
	fixture.items(t, 2)

	// Only the healthy resource's checks advance while the other fails.
	fixture.resources = []string{"items"}
	fixture.watch(t)

	fixture.assertDid(t, "2")
	assertProcessed(t, downProcessed)

	st, err := sqlite.OpenExisting(fixture.db, fixture.name)
	if err != nil {
		t.Fatal(err)
	}

	failing, err := st.CheckErrors(t.Context())
	_ = st.Close()

	if err != nil {
		t.Fatal(err)
	}

	if len(failing) != 1 || failing[0].Name != "down" || !strings.Contains(failing[0].Message, "upstream is down") {
		t.Errorf("check errors = %+v, want only down's", failing)
	}

	err = os.Remove(broken)
	if err != nil {
		t.Fatal(err)
	}

	fixture.resources = []string{"items", "down"}
	fixture.watch(t)

	assertProcessed(t, downProcessed, "2")
	fixture.assertDid(t, "2")
}

func assertProcessed(t *testing.T, path string, want ...string) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir()-scoped file this test's pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.Fields(string(data)); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("%s processed %v, want %v", path, got, want)
	}
}

// waitForCheckError waits until the daemon has recorded a failed check of the resource.
func waitForCheckError(t *testing.T, fixture *watchFixture, resource string) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)

	for time.Now().Before(deadline) {
		st, err := sqlite.OpenExisting(fixture.db, fixture.name)
		if err != nil {
			t.Fatal(err)
		}

		failing, err := st.CheckErrors(t.Context())
		_ = st.Close()

		if err != nil {
			t.Fatal(err)
		}

		for _, failure := range failing {
			if failure.Name == resource {
				return
			}
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("the daemon never recorded a failed check of %q", resource)
}
