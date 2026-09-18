package storetest

// webhook_deliveries: a delivery is a version, and its payload lives and dies with it.

import (
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func delivery(id string) store.Delivery {
	return store.Delivery{
		Version: map[string]any{"id": id, "event": "push"},
		Body:    []byte(`{"after":"` + id + `"}`),
		Headers: map[string]string{"x-github-event": "push"},
	}
}

func pendingJobs(t *testing.T, st store.Store) []string {
	t.Helper()

	rows, err := st.ListTriggerQueue(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}

	var pending []string

	for _, row := range rows {
		if row.Status == "pending" {
			pending = append(pending, row.JobName)
		}
	}

	return pending
}

func encoded(t *testing.T, version map[string]any) string {
	t.Helper()

	encoded, err := store.EncodeVersion(version)
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}

func recordedPayload(t *testing.T, st store.Store, version string) (store.Delivery, bool) {
	t.Helper()

	got, found, err := st.Delivery(context.Background(), "push", version)
	if err != nil {
		t.Fatal(err)
	}

	return got, found
}

// TestADeliveryIsAVersionWithItsPayload: the delivery is filed as history a job reads, and its payload is kept beside it.
func (s suite) TestADeliveryIsAVersionWithItsPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	recorded, err := st.RecordDelivery(ctx, "push", delivery("a"), store.Dispatch{Jobs: []string{"build"}}, 0)
	if err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v, want a first delivery recorded", recorded, err)
	}

	version := encoded(t, delivery("a").Version)

	versions, err := st.ResourceVersionsJSON(ctx, "push")
	if err != nil || len(versions) != 1 || versions[0] != version {
		t.Errorf("versions = %v (err %v), want the delivery as the resource's one version: a job reads history, and without from_check a get would go looking for a check", versions, err)
	}

	got, found := recordedPayload(t, st, version)
	if !found || string(got.Body) != `{"after":"a"}` || got.Headers["x-github-event"] != "push" {
		t.Errorf("payload = %+v found=%v, want the body and headers as delivered", got, found)
	}
}

// TestADeliveryDispatchesInTheSameCall: the jobs and the current version move with the record, not after it, so no failure between them can leave a delivery recorded and never built.
func (s suite) TestADeliveryDispatchesInTheSameCall(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	_, err := st.RecordDelivery(ctx, "push", delivery("a"), store.Dispatch{Jobs: []string{"build"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if pending := pendingJobs(t, st); len(pending) != 1 || pending[0] != "build" {
		t.Errorf("pending = %v, want build queued by the same call", pending)
	}

	version := encoded(t, delivery("a").Version)

	checked, found, err := st.LastChecked(ctx, "push")
	if err != nil || !found || checked.Version != version {
		t.Errorf("current version = %q (found=%v err=%v), want the delivery: it is what the resource page shows", checked.Version, found, err)
	}
}

// TestARedeliveryChangesNothing: a sender retrying, or somebody pressing redeliver, lands on the version already recorded and queues nothing.
func (s suite) TestARedeliveryChangesNothing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	_, err := st.RecordDelivery(ctx, "push", delivery("a"), store.Dispatch{Jobs: []string{"build"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	_, _, found, err := st.ClaimNextJob(ctx)
	if err != nil || !found {
		t.Fatalf("claim: found=%v err=%v", found, err)
	}

	again := delivery("a")
	again.Body = []byte("replayed")

	recorded, err := st.RecordDelivery(ctx, "push", again, store.Dispatch{Jobs: []string{"build"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	if recorded {
		t.Error("a redelivery reported as new")
	}

	if pending := pendingJobs(t, st); len(pending) != 0 {
		t.Errorf("pending = %v after a redelivery, want nothing queued", pending)
	}

	got, _ := recordedPayload(t, st, encoded(t, again.Version))
	if string(got.Body) != `{"after":"a"}` {
		t.Errorf("payload = %s, want the first delivery's: the version a job may already have built must keep meaning what it meant", got.Body)
	}
}

// TestPruningAVersionTakesItsPayload: version_history: is the only bound on stored payloads, which may carry personal data.
func (s suite) TestPruningAVersionTakesItsPayload(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, id := range []string{"a", "b"} {
		_, err := st.RecordDelivery(ctx, "push", delivery(id), store.Dispatch{}, 1)
		if err != nil {
			t.Fatal(err)
		}
	}

	if _, found := recordedPayload(t, st, encoded(t, delivery("a").Version)); found {
		t.Error("the pruned version's payload is still stored")
	}

	if _, found := recordedPayload(t, st, encoded(t, delivery("b").Version)); !found {
		t.Error("the newest delivery's payload was pruned, want kept")
	}
}

// TestAHeldDeliveryIsRecordedButNotDispatched: a paused pipeline keeps the delivery and leaves the current version behind it, which is how the first poll after unpause knows there is something to build.
func (s suite) TestAHeldDeliveryIsRecordedButNotDispatched(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	recorded, err := st.RecordDelivery(ctx, "push", delivery("a"), store.Dispatch{Jobs: []string{"build"}, Held: true}, 0)
	if err != nil || !recorded {
		t.Fatalf("recorded=%v err=%v, want the delivery kept", recorded, err)
	}

	if pending := pendingJobs(t, st); len(pending) != 0 {
		t.Errorf("pending = %v, want nothing queued while held", pending)
	}

	_, found, err := st.LastChecked(ctx, "push")
	if err != nil {
		t.Fatal(err)
	}

	if found {
		t.Error("a held delivery moved the current version; the poll after unpause would see nothing to dispatch")
	}
}

// TestTheCheckedVersionMovesOnlyFromWhatWasRead: a compare-and-set that loses leaves the newer version in place, including when the expectation was that none existed.
func (s suite) TestTheCheckedVersionMovesOnlyFromWhatWasRead(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for _, step := range []struct {
		expected string
		found    bool
		next     string
		moves    bool
	}{
		{"", false, "a", true},
		{"", false, "b", false},
		{"stale", true, "b", false},
		{"a", true, "b", true},
	} {
		moved, err := st.CompareAndSetCheckedVersion(ctx, "push", step.expected, step.found, step.next)
		if err != nil || moved != step.moves {
			t.Errorf("from %q (found=%v) to %q: moved=%v err=%v, want moved=%v", step.expected, step.found, step.next, moved, err, step.moves)
		}
	}

	checked, _, err := st.LastChecked(ctx, "push")
	if err != nil || checked.Version != "b" {
		t.Errorf("checked = %q (err %v), want b", checked.Version, err)
	}
}
