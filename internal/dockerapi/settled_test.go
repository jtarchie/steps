package dockerapi

import (
	"context"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

// The engine client sends an exit on an unbuffered channel, so a wait that answers just as the bound elapses is only finished once somebody receives it.
func TestSettledReceivesAnExitThatArrivesAsTheBoundElapses(t *testing.T) {
	waitCtx, cancel := context.WithCancel(t.Context())
	exited := make(chan container.WaitResponse)
	sent := make(chan struct{})

	go func() {
		defer close(sent)

		<-waitCtx.Done()
		exited <- container.WaitResponse{StatusCode: 5}
	}()

	cancel()

	died, code, err := settled(t.Context(), waitCtx, "c1", exited, make(chan error, 1))
	if err != nil || !died || code != 5 {
		t.Errorf("settled = %v, %d, %v; want the exit that arrived: true, 5, nil", died, code, err)
	}

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait is still blocked sending its exit: nothing received it")
	}
}

func TestSettledReadsTheBoundElapsingAsStillUp(t *testing.T) {
	waitCtx, cancel := context.WithCancel(t.Context())
	cancel()

	failed := make(chan error, 1)
	failed <- context.Canceled

	died, _, err := settled(t.Context(), waitCtx, "c1", make(chan container.WaitResponse), failed)
	if err != nil || died {
		t.Errorf("settled = %v, %v; want still up and no error", died, err)
	}
}
