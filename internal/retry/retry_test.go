package retry

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDoFailsAfterExhaustingAttempts(t *testing.T) {
	t.Parallel()

	calls := 0

	err := Do(context.Background(), 2, func(_ int) error {
		calls++

		return errors.New("boom")
	})
	if err == nil {
		t.Fatal("expected an error after exhausting attempts")
	}

	if calls != 2 {
		t.Errorf("calls = %d, want 2", calls)
	}
}

// TestDoStopsOnStopError checks that a Stop-wrapped error ends the loop after
// one call and comes back unwrapped, so callers see the original error chain
// (and their own errors.Is/errors.As checks) exactly as if Stop were absent.
func TestDoStopsOnStopError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("deadline")
	calls := 0

	err := Do(context.Background(), 3, func(_ int) error {
		calls++

		return Stop(fmt.Errorf("task %q: %w", "slow", sentinel))
	})

	if calls != 1 {
		t.Errorf("calls = %d, want 1 (a Stop must not be retried)", calls)
	}

	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to unwrap to the sentinel", err)
	}

	var stop *stopError
	if errors.As(err, &stop) {
		t.Error("the stopError marker escaped Do; it must be unwrapped before returning")
	}

	if want := `task "slow": deadline`; err.Error() != want {
		t.Errorf("err.Error() = %q, want %q", err.Error(), want)
	}
}

// TestStopOnDeadline covers the four cases the three retry scaffolds rely on:
// only *our* attempt deadline stops the loop.
func TestStopOnDeadline(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")

	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	aborted, cancelAborted := context.WithCancel(context.Background())
	cancelAborted()

	tests := map[string]struct {
		ctx, attemptCtx context.Context //nolint:containedctx // table-driven inputs, not stored state
		err             error
		wantStop        bool
	}{
		"attempt deadline expired": {context.Background(), expired, boom, true},
		"attempt still live":       {context.Background(), context.Background(), boom, false},
		"job aborted":              {aborted, expired, boom, false},
		"no error":                 {context.Background(), expired, nil, false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := StopOnDeadline(test.ctx, test.attemptCtx, test.err)

			var stop *stopError
			if errors.As(got, &stop) != test.wantStop {
				t.Errorf("StopOnDeadline(...) stop = %v, want %v", !test.wantStop, test.wantStop)
			}

			if !errors.Is(got, test.err) {
				t.Errorf("StopOnDeadline(...) = %v, want it to carry %v", got, test.err)
			}
		})
	}
}

// TestStopIsNilSafe guards the nil case, since callers wrap an error that may
// be nil on a successful attempt.
func TestStopIsNilSafe(t *testing.T) {
	t.Parallel()

	err := Stop(nil)
	if err != nil {
		t.Errorf("Stop(nil) = %v, want nil", err)
	}
}
