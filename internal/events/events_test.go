package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestBusFansOutToSubscribersAndSink covers the bus's whole job: every
// published event reaches every subscriber and the sink, in order, stamped
// with a monotonic sequence.
func TestBusFansOutToSubscribersAndSink(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		sunk   []Event
		wgSink sync.WaitGroup
	)

	wgSink.Add(3)

	bus := New(func(e Event) {
		mu.Lock()
		sunk = append(sunk, e)
		mu.Unlock()
		wgSink.Done()
	})

	first, cancelFirst := bus.Subscribe()
	defer cancelFirst()

	second, cancelSecond := bus.Subscribe()
	defer cancelSecond()

	for _, name := range []string{"a", "b", "c"} {
		bus.Publish(Event{Type: TypeStepStarted, StepName: name})
	}

	assertReceivesInOrder(t, first)
	assertReceivesInOrder(t, second)

	wgSink.Wait()
	bus.Close()

	mu.Lock()
	defer mu.Unlock()

	if len(sunk) != 3 {
		t.Fatalf("sink saw %d events, want 3", len(sunk))
	}

	if sunk[0].Seq >= sunk[1].Seq || sunk[1].Seq >= sunk[2].Seq {
		t.Errorf("sequence numbers not monotonic: %d %d %d", sunk[0].Seq, sunk[1].Seq, sunk[2].Seq)
	}
}

// TestSlowSubscriberNeverBlocksPublish is the guarantee the pipeline depends
// on: a browser that stopped reading must not be able to stall a running job.
// Publishing far past a subscriber's buffer has to keep returning.
func TestSlowSubscriberNeverBlocksPublish(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	_, cancel := bus.Subscribe()
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range subscriberBuffer * 4 {
			bus.Publish(Event{Type: TypeStepStarted})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a subscriber that never read")
	}
}

// TestNilBusIsUsable pins the no-op contract every execution package relies
// on: off the web path there is no bus, and nothing may panic.
func TestNilBusIsUsable(t *testing.T) {
	t.Parallel()

	var bus *Bus

	bus.Publish(Event{Type: TypeStepStarted})
	bus.Close()

	ch, cancel := bus.Subscribe()
	cancel()

	if _, open := <-ch; open {
		t.Error("a nil bus handed out an open subscription")
	}

	// The context helpers behave the same way.
	ctx := context.Background()
	Publish(ctx, Event{Type: TypeStepStarted})

	if got := FromContext(ctx); got != nil {
		t.Errorf("FromContext on a bare context = %v, want nil", got)
	}

	if id := RunID(ctx); id != "" {
		t.Errorf("RunID on a bare context = %q, want empty", id)
	}
}

// TestRunIDRoundTrip covers the seam that lets internal/agent stamp events
// with a run id owned by internal/pipeline.
func TestRunIDRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := WithRunID(context.Background(), "run-42")

	if got := RunID(ctx); got != "run-42" {
		t.Errorf("RunID = %q, want run-42", got)
	}
}

// TestUnsubscribeStopsDelivery checks a canceled subscription is closed and
// removed, so a finished stream stops costing anything.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	ch, cancel := bus.Subscribe()
	cancel()

	if _, open := <-ch; open {
		t.Error("cancel did not close the subscription channel")
	}

	// Publishing afterwards must not panic on the closed channel.
	bus.Publish(Event{Type: TypeStepFinished})
}

// assertReceivesInOrder drains one subscription and checks it saw every
// event, in order, stamped.
func assertReceivesInOrder(t *testing.T, ch <-chan Event) {
	t.Helper()

	for i, want := range []string{"a", "b", "c"} {
		select {
		case got := <-ch:
			if got.StepName != want {
				t.Errorf("event %d = %q, want %q", i, got.StepName, want)
			}

			if got.Seq == 0 {
				t.Error("published event carries no sequence number")
			}

			if got.At.IsZero() {
				t.Error("published event carries no timestamp")
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber did not receive event %d", i)
		}
	}
}

// TestPublishDuringCloseIsSafe covers the shutdown path: `steps web` closes
// its bus while a job it started is still publishing. Before the close flag,
// the concurrent send raced Close's close(sink) and panicked with "send on a
// closed channel", taking the process down instead of shutting it down.
func TestPublishDuringCloseIsSafe(t *testing.T) {
	t.Parallel()

	for range 50 {
		bus := New(func(Event) {})

		var wg sync.WaitGroup

		wg.Add(2)

		go func() {
			defer wg.Done()

			for range 200 {
				bus.Publish(Event{Type: TypeStepStarted})
			}
		}()

		go func() {
			defer wg.Done()

			bus.Close()
		}()

		wg.Wait()

		// Idempotent: a second Close must not double-close the sink.
		bus.Close()

		// And a publish after close is a no-op, not a panic.
		bus.Publish(Event{Type: TypeStepFinished})
	}
}

// TestObserverSeesEveryEventInOrder is the observer's whole reason to exist:
// a subscriber may be dropped from, and a terminal that drops a line has lost
// it for good. Concurrent publishers and a slow observer still deliver every
// event, in sequence order.
func TestObserverSeesEveryEventInOrder(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	var seen []int64

	cancel := bus.Observe(func(event Event) {
		if len(seen)%97 == 0 {
			time.Sleep(time.Millisecond)
		}

		seen = append(seen, event.Seq)
	})
	defer cancel()

	const publishers, each = 8, 100

	var wg sync.WaitGroup

	for range publishers {
		wg.Go(func() {
			for range each {
				bus.Publish(Event{Type: TypeStepNote})
			}
		})
	}

	wg.Wait()

	if len(seen) != publishers*each {
		t.Fatalf("observer saw %d events, want %d — it must never be dropped from", len(seen), publishers*each)
	}

	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("observer saw seq %d after %d, want sequence order", seen[i], seen[i-1])
		}
	}
}

// TestObserverCancelStopsDelivery checks a cancelled observer is called no
// more, and that a nil bus hands out a usable cancel.
func TestObserverCancelStopsDelivery(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	calls := 0
	cancel := bus.Observe(func(Event) { calls++ })

	bus.Publish(Event{})
	cancel()
	bus.Publish(Event{})

	if calls != 1 {
		t.Errorf("observer called %d times, want 1", calls)
	}

	var none *Bus

	none.Observe(func(Event) { t.Error("a nil bus called its observer") })()
}

// TestNoteStampsTheRunAndStep covers the seam every package without a step
// identity of its own publishes a note through: the run and the step come
// off the context, so a note lands under the step that was running.
func TestNoteStampsTheRunAndStep(t *testing.T) {
	t.Parallel()

	bus := New(nil)

	var got []Event

	defer bus.Observe(func(event Event) { got = append(got, event) })()

	ctx := WithRunID(WithBus(context.Background(), bus), "run-7")
	Note(WithStepID(ctx, 3), NoteWarn, "pulling image alpine")
	Note(ctx, NoteInfo, "job-level")

	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}

	step := got[0]
	if step.Type != TypeStepNote || step.RunID != "run-7" || step.StepID != 3 ||
		step.Status != NoteWarn || step.Text != "pulling image alpine" {
		t.Errorf("step note = %+v", step)
	}

	if job := got[1]; job.StepID != 0 || job.StepIndex != -1 || job.Status != NoteInfo {
		t.Errorf("job note = %+v, want no step and StepIndex -1", job)
	}
}
