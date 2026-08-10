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
