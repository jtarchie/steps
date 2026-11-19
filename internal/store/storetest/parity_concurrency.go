package storetest

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

const crowd = 16

// TestConcurrentResumePressesQueueOneResume: the guard is a NOT EXISTS over rows no unique index covers, so two presses that each read before the other writes both queue, and the run is resumed twice.
func (s suite) TestConcurrentResumePressesQueueOneResume(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	handles := []store.Store{s.open(t, "test"), s.open(t, "test")}

	const runs = 10

	err := handles[0].SyncJobLimits(ctx, nil, map[string]int{"build": runs * crowd})
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	for n := range runs {
		runID := fmt.Sprintf("FAILED%02d", n)
		seedRuns(t, handles[0], "build", runID)

		err = handles[0].FinishRun(ctx, runID, store.RunFailed)
		if err != nil {
			t.Fatalf("FinishRun: %v", err)
		}

		concurrently(crowd, handles, func(st store.Store, _ int) {
			err := st.EnqueueResumeJob(ctx, "build", "resume (web)", runID, "")
			if err != nil {
				t.Errorf("EnqueueResumeJob: %v", err)
			}
		})
	}

	claimed := map[string]int{}
	for _, runID := range claimEveryResume(t, handles[0]) {
		claimed[runID]++
	}

	assertClaimedExactlyOnce(t, claimed, runs)
}

// TestConcurrentCacheEntryRecordsLeaveOneEntry: a re-record deletes the key's rows and inserts its own, and two writers that both delete before either inserts collide on the key or leave a mix of both.
func (s suite) TestConcurrentCacheEntryRecordsLeaveOneEntry(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	handles := []store.Store{s.open(t, "test"), s.open(t, "test")}

	for round := range 10 {
		key := fmt.Sprintf("key-%d", round)

		concurrently(crowd, handles, func(st store.Store, i int) {
			_, err := st.RecordCacheEntry(ctx, store.CacheStep, key, map[string]store.CachedTree{"out": {Digest: strconv.Itoa(i)}, "logs": {Digest: strconv.Itoa(i)}}, 100)
			if err != nil {
				t.Errorf("RecordCacheEntry: %v", err)
			}
		})

		got, err := handles[0].CacheEntry(ctx, store.CacheStep, key)
		if err != nil {
			t.Fatalf("CacheEntry: %v", err)
		}

		if len(got) != 2 || got["out"] != got["logs"] {
			t.Errorf("round %d: %s = %v, want one writer's whole entry", round, key, got)
		}
	}
}

// TestConcurrentRemembersHonourTheCap: the cap deletes past the newest limit it can see, and a rememberer that cannot see another's uncommitted entry keeps it as well as its own.
func (s suite) TestConcurrentRemembersHonourTheCap(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	handles := []store.Store{s.open(t, "test"), s.open(t, "test")}

	const limit = 3

	for round := range 10 {
		scope := fmt.Sprintf("U%d", round)

		concurrently(crowd, handles, func(st store.Store, i int) {
			_, _, err := st.Remember(ctx, store.Memory{Scope: scope, Text: fmt.Sprintf("fact %d", i)}, limit)
			if err != nil {
				t.Errorf("Remember: %v", err)
			}
		})

		if got := memoryTexts(ctx, t, handles[0], scope); len(got) != limit {
			t.Errorf("round %d: %d memories kept under a cap of %d: %v", round, len(got), limit, got)
		}
	}
}
