package storetest

// The activity log: one per database, written through a pipeline's handle, outliving the pipeline it names.

import (
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// TestActivityIsNewestFirstAndFilters: every field narrows, and they combine.
func (s suite) TestActivityIsNewestFirstAndFilters(t *testing.T) {
	t.Parallel()

	app, other := s.open(t, "app"), s.open(t, "other")

	recordActivity(t, app, store.ActivityEntry{Actor: "local:alex", Action: "trigger", Target: "build"}, 0)
	recordActivity(t, app, store.ActivityEntry{Actor: "local:sam", Action: "trigger", Target: "test"}, 0)
	recordActivity(t, other, store.ActivityEntry{Actor: "local:alex", Action: "pause"}, 0)

	all := readActivity(t, app, store.ActivityFilter{})
	if len(all) != 3 || all[0].Action != "pause" || all[0].Pipeline != "other" || all[2].Target != "build" || all[2].Pipeline != "app" {
		t.Fatalf("Activity = %+v; want three entries, newest first, each naming its pipeline", all)
	}

	if all[2].At.IsZero() || all[2].ID == 0 {
		t.Errorf("an entry came back without its id or time: %+v", all[2])
	}

	for _, tc := range []struct {
		filter store.ActivityFilter
		want   int
	}{
		{store.ActivityFilter{Pipeline: "app"}, 2},
		{store.ActivityFilter{Actor: "local:alex"}, 2},
		{store.ActivityFilter{Action: "trigger"}, 2},
		{store.ActivityFilter{Pipeline: "app", Actor: "local:alex"}, 1},
		{store.ActivityFilter{Limit: 1}, 1},
		{store.ActivityFilter{Target: "build"}, 1},
		{store.ActivityFilter{Actions: []string{"pause", "nothing"}}, 1},
		{store.ActivityFilter{Actions: []string{"pause", "trigger"}}, 3},
		{store.ActivityFilter{Since: time.Now().Add(time.Hour)}, 0},
		{store.ActivityFilter{Since: time.Now().Add(-time.Hour)}, 3},
		{store.ActivityFilter{Before: all[0].ID}, 2},
	} {
		if got := readActivity(t, app, tc.filter); len(got) != tc.want {
			t.Errorf("Activity(%+v) = %d entries, want %d", tc.filter, len(got), tc.want)
		}
	}
}

// TestActivityIsCappedAcrossPipelines: the newest limit entries survive, whichever pipeline wrote them.
func (s suite) TestActivityIsCappedAcrossPipelines(t *testing.T) {
	t.Parallel()

	app, other := s.open(t, "app"), s.open(t, "other")

	recordActivity(t, app, store.ActivityEntry{Action: "one"}, 2)
	recordActivity(t, other, store.ActivityEntry{Action: "two"}, 2)
	recordActivity(t, app, store.ActivityEntry{Action: "three"}, 2)

	got := readActivity(t, app, store.ActivityFilter{})
	if len(got) != 2 || got[0].Action != "three" || got[1].Action != "two" {
		t.Fatalf("Activity = %+v; want the newest two", got)
	}
}

// TestActivityOutlivesItsPipeline: destroying a pipeline keeps the record of what was done to it, including the destruction, under the name it had.
func (s suite) TestActivityOutlivesItsPipeline(t *testing.T) {
	t.Parallel()

	doomed, survivor := s.open(t, "doomed"), s.open(t, "survivor")

	recordActivity(t, doomed, store.ActivityEntry{Action: "destroy"}, 0)

	err := doomed.Delete(ctxFor(t))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := readActivity(t, survivor, store.ActivityFilter{Pipeline: "doomed"})
	if len(got) != 1 || got[0].Action != "destroy" {
		t.Fatalf("Activity(doomed) after the destroy = %+v; want the destroy kept", got)
	}
}

// TestActivityFollowsARename: a pipeline's history answers to its new name.
func (s suite) TestActivityFollowsARename(t *testing.T) {
	t.Parallel()

	app := s.open(t, "before")

	recordActivity(t, app, store.ActivityEntry{Action: "trigger"}, 0)

	err := app.Rename(ctxFor(t), "after")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}

	if got := readActivity(t, app, store.ActivityFilter{Pipeline: "after"}); len(got) != 1 {
		t.Fatalf("Activity(after) = %+v; want the entry recorded before the rename", got)
	}
}

func recordActivity(t *testing.T, st store.Store, entry store.ActivityEntry, limit int) {
	t.Helper()

	err := st.RecordActivity(ctxFor(t), entry, limit)
	if err != nil {
		t.Fatalf("RecordActivity(%+v): %v", entry, err)
	}
}

func readActivity(t *testing.T, st store.Store, filter store.ActivityFilter) []store.ActivityEntry {
	t.Helper()

	got, err := st.Reader().Activity(ctxFor(t), filter)
	if err != nil {
		t.Fatalf("Activity(%+v): %v", filter, err)
	}

	return got
}
