package outcome

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassify(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		canceled bool
		err      error
		want     Class
	}{
		{"nil error is succeeded", false, nil, Succeeded},
		{"marked failure is failed", false, Fail(errors.New("exit 1")), Failed},
		{"wrapped marked failure is failed", false, fmt.Errorf("step 2: %w", Fail(errors.New("exit 1"))), Failed},
		{"plain error is errored", false, errors.New("docker down"), Errored},
		{"canceled ctx wins over a marked failure", true, Fail(errors.New("exit 1")), Aborted},
		{"canceled ctx with any error is aborted", true, errors.New("boom"), Aborted},
		{
			// An internal per-step timeout must not read as an abort while the
			// job context is still live — Classify keys on ctx.Err(), not on
			// errors.Is(err, context.DeadlineExceeded).
			name: "internal deadline with live parent ctx is errored",
			err:  fmt.Errorf("agent step: %w", context.DeadlineExceeded),
			want: Errored,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tt.canceled {
				cancel()
			}

			got := Classify(ctx, tt.err)
			if got != tt.want {
				t.Errorf("Classify(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestFailNilSafe(t *testing.T) {
	t.Parallel()

	if Fail(nil) != nil {
		t.Error("Fail(nil) should be nil")
	}
}
