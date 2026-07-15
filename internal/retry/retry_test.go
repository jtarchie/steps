package retry

import (
	"context"
	"errors"
	"testing"
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
