package storetest

// What stays true when several writers reach one pipeline at once.
//
// sqlite serializes every write transaction on a file, so these hold there by
// construction; a server database answers several connections at once, and
// each read-then-write below is a race unless the driver serializes it itself.
// Two handles on one pipeline, so a driver's per-handle pool is not what
// happens to serialize them.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// concurrently runs fn from n goroutines released at once, spread over the
// handles, and waits for all of them.
func concurrently(n int, handles []store.Store, fn func(st store.Store, i int)) {
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
	)

	for i := range n {
		wg.Go(func() {
			<-start
			fn(handles[i%len(handles)], i)
		})
	}

	close(start)
	wg.Wait()
}

// TestConcurrentClaimersHonourSerialGroups: two jobs sharing a serial group,
// both pending, and a crowd of claimers. Exactly one may be admitted.
//
// A claim counts what is already running and then takes a row. Two claimers
// that each count before the other's take is visible both admit, and a serial
// group is two deploys to one target at once.
func (s suite) TestConcurrentClaimersHonourSerialGroups(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	handles := []store.Store{s.open(t, "test"), s.open(t, "test")}

	err := handles[0].SyncJobLimits(ctx, map[string][]string{"deploy-a": {"target"}, "deploy-b": {"target"}}, nil)
	if err != nil {
		t.Fatalf("SyncJobLimits: %v", err)
	}

	for round := range 20 {
		mustEnqueueJob(t, handles[0], "deploy-a", "r")
		mustEnqueueJob(t, handles[0], "deploy-b", "r")

		var (
			mu      sync.Mutex
			claimed []int64
		)

		concurrently(16, handles, func(st store.Store, _ int) {
			id, _, found, err := st.ClaimNextJob(ctx)
			if err != nil {
				t.Errorf("ClaimNextJob: %v", err)

				return
			}

			if found {
				mu.Lock()
				claimed = append(claimed, id)
				mu.Unlock()
			}
		})

		if len(claimed) != 1 {
			t.Fatalf("round %d: %d claims admitted from one serial group, want exactly 1", round, len(claimed))
		}

		// Drain: finish the one admitted, then the other it held back.
		err = handles[0].CompleteJob(ctx, claimed[0], "succeeded", nil)
		if err != nil {
			t.Fatalf("CompleteJob: %v", err)
		}

		id, _ := mustClaimJob(t, handles[0], "")

		err = handles[0].CompleteJob(ctx, id, "succeeded", nil)
		if err != nil {
			t.Fatalf("CompleteJob: %v", err)
		}
	}
}

// TestConcurrentVersionsGetDistinctOrder: check_order is discovery order, and
// it is assigned from the highest order already recorded. Two writers reading
// that highest value at once hand two versions one order — and "latest", the
// cursor and the prune all read the order as a total one.
func (s suite) TestConcurrentVersionsGetDistinctOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	handles := []store.Store{s.open(t, "test"), s.open(t, "test")}

	const writers = 24

	versions := make([]map[string]any, writers)
	encoded := make([]string, writers)

	for i := range writers {
		versions[i] = map[string]any{"ref": fmt.Sprintf("v%d", i)}
		encoded[i] = mustEncode(t, versions[i])
	}

	var failures atomic.Int64

	concurrently(writers, handles, func(st store.Store, i int) {
		// Every path that numbers a version: a check's report, a version a
		// run used, and one a job went green on.
		var err error

		switch i % 3 {
		case 0:
			_, err = st.RecordVersions(ctx, "repo", []map[string]any{versions[i]}, 0)
		case 1:
			_, err = st.RecordVersionOrder(ctx, "repo", encoded[i])
		default:
			err = st.RecordPassedVersion(ctx, "build", "repo", encoded[i], "b1")
		}

		if err != nil {
			failures.Add(1)
			t.Errorf("writer %d: %v", i, err)
		}
	})

	if failures.Load() > 0 {
		return
	}

	assertDistinctOrders(t, handles[0], "repo", writers)
}

func assertDistinctOrders(t *testing.T, st store.Store, resource string, want int) {
	t.Helper()

	orders, err := st.VersionOrders(context.Background(), resource)
	if err != nil {
		t.Fatalf("VersionOrders: %v", err)
	}

	if len(orders) != want {
		t.Fatalf("%d versions recorded, want %d", len(orders), want)
	}

	seen := map[int64]string{}

	for version, order := range orders {
		if other, dup := seen[order]; dup {
			t.Errorf("%s and %s share check_order %d — two writers read the same highest order", version, other, order)
		}

		seen[order] = version
	}
}
