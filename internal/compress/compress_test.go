package compress

import (
	"errors"
	"io"
	"testing"
)

// TestPackReportsTheWritersFailure: a tree that failed to pack partway must not be reported as packed — the caller would upload the truncated stream under the tree's digest.
func TestPackReportsTheWritersFailure(t *testing.T) {
	broken := errors.New("tree vanished mid-walk")

	err := Pack(io.Discard, true, func(io.Writer) error { return broken })
	if !errors.Is(err, broken) {
		t.Fatalf("Pack = %v, want the writer's own error", err)
	}
}
