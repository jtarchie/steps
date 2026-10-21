package venue

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// The name search reads a replica that lags the create: a first empty answer is not "nothing to delete".
func TestDOLostCreateIsFoundDespiteListLag(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.createStatus, fake.createThenFail = http.StatusInternalServerError, true
	fake.listLag = 2

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil {
		t.Fatal("acquire succeeded on a lost create")
	}

	if _, deletes, live := fake.snapshot(); live != 0 || len(deletes) != 1 {
		t.Errorf("after a lost create behind list lag: %d left, deletes %v — want the droplet found by a later search and deleted", live, deletes)
	}
}

// A refusal made nothing, so the name search is not repeated waiting for a droplet that will never show.
func TestDORefusedCreateDoesNotWaitOnTheList(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.createStatus = http.StatusUnprocessableEntity
	fake.listLag = 1 << 30

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(context.Background(), worker)
	if err == nil {
		t.Fatal("acquire succeeded on a refused create")
	}

	if fake.listLag != 1<<30-1 {
		t.Errorf("name searches = %d, want exactly one after a refusal", 1<<30-fake.listLag)
	}
}

// Cancelled while DigitalOcean is accepting the create: the droplet exists, and must be deleted rather than returned to a caller that is gone.
func TestDOCancelledMidCreateDeletesTheDroplet(t *testing.T) {
	fake := newFakeDO(t)
	fake.noBoot = true
	fake.listLag = 0

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake.onCreate = cancel

	worker, _ := ParseWorker("do://launch/")

	_, _, err := acquire(ctx, worker)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire = %v, want the cancellation", err)
	}

	if _, deletes, live := fake.snapshot(); live != 0 || len(deletes) != 1 {
		t.Errorf("after a cancel mid-create: %d left, deletes %v — want the droplet deleted", live, deletes)
	}
}
