package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

const requestedPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: "echo '[{\"n\":\"1\"}]'"
    in: "true"
resources:
- name: items
  type: feed
  source: {}
jobs:
- name: build
  plan:
  - get: items
`

func checkedNames(t *testing.T, st store.Store) []string {
	t.Helper()

	rows, err := st.CheckedResources(t.Context())
	if err != nil {
		t.Fatalf("CheckedResources: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	return names
}

// pollAsked runs the loop with an interval nothing reaches, asks it for each name, and stops it after wait.
func pollAsked(t *testing.T, st store.Store, cfgYAML string, wait time.Duration, names ...string) {
	t.Helper()

	cfg := loadConfig(t, t.TempDir(), cfgYAML)

	ctx, cancel := context.WithCancel(t.Context())
	requests := NewRequests()
	done := make(chan error, 1)

	go func() { done <- Poll(ctx, staticConfig(cfg), st, time.Hour, requests) }()

	for _, name := range names {
		if !requests.Check(name) {
			t.Fatalf("Check(%q) was refused by an empty backlog", name)
		}
	}

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) && len(checkedNames(t, st)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()

	err := <-done
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
}

// TestARequestedCheckReachesAResourceNothingPolls: no get here says trigger: true, so the loop's own ticks check nothing — and a person asking still gets the check.
func TestARequestedCheckReachesAResourceNothingPolls(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())

	pollAsked(t, st, requestedPipeline, 10*time.Second, "gone", "items")

	if got := checkedNames(t, st); len(got) != 1 || got[0] != "items" {
		t.Errorf("checked %v, want items — and a name removed by a set skipped rather than stopping the loop", got)
	}
}

// TestAPausedPipelineChecksNothingAsked: a pause stops checks, and a request is a check.
func TestAPausedPipelineChecksNothingAsked(t *testing.T) {
	t.Parallel()

	st := mustOpenStore(t, t.TempDir())

	err := st.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	pollAsked(t, st, requestedPipeline, 300*time.Millisecond, "items")

	if got := checkedNames(t, st); len(got) != 0 {
		t.Errorf("a paused pipeline checked %v", got)
	}
}

// TestRequestsRefuseOnceTheBacklogIsFull: a request is a nudge, and a full backlog says no rather than blocking the handler that asked.
func TestRequestsRefuseOnceTheBacklogIsFull(t *testing.T) {
	t.Parallel()

	requests := NewRequests()

	for range requestBacklog {
		if !requests.Check("items") {
			t.Fatal("refused before the backlog was full")
		}
	}

	if requests.Check("items") {
		t.Error("a full backlog took another request")
	}
}
