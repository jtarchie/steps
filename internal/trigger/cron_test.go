package trigger

import (
	"context"
	"slices"
	"testing"
	"time"

	rsrc "github.com/jtarchie/steps/internal/resource"
)

// TestPollOnceCronMintsOnlyWhenASlotPasses is the seam from the schedule to
// the queue: the version a poll mints becomes the cursor the next poll is
// handed, and only a slot between the two enqueues the job. The clock is
// driven, so the hour it walks through costs nothing.
func TestPollOnceCronMintsOnlyWhenASlotPasses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true
resources:
- name: hourly
  type: cron
  source:
    expression: "0 * * * *"
jobs:
- name: build
  plan:
  - get: hourly
    trigger: true
`)
	st := mustOpenStore(t, dir)

	now := time.Date(2026, 9, 25, 10, 30, 0, 0, time.UTC)
	ctx := rsrc.WithNow(context.Background(), func() time.Time { return now })

	poll := func(at time.Time) []string {
		t.Helper()

		now = at

		enqueued, err := pollOnce(ctx, cfg, st)
		if err != nil {
			t.Fatalf("pollOnce at %s: %v", at, err)
		}

		return enqueued
	}

	steps := []struct {
		at   time.Time
		want []string
		why  string
	}{
		{time.Date(2026, 9, 25, 10, 30, 0, 0, time.UTC), []string{"build"}, "a first check, with the ten o'clock slot in the last hour"},
		{time.Date(2026, 9, 25, 10, 45, 0, 0, time.UTC), nil, "no slot since the version minted at half past"},
		{time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC), []string{"build"}, "the eleven o'clock slot"},
		{time.Date(2026, 9, 25, 11, 0, 30, 0, time.UTC), nil, "the poll after minting, inside the same slot"},
		{time.Date(2026, 9, 25, 13, 5, 0, 0, time.UTC), []string{"build"}, "two slots passed while nothing polled: one build, not two"},
	}

	for _, step := range steps {
		if got := poll(step.at); !slices.Equal(got, step.want) {
			t.Errorf("poll at %s enqueued %v, want %v — %s", step.at.Format(time.TimeOnly), got, step.want, step.why)
		}
	}
}
