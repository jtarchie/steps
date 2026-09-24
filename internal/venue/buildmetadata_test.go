package venue

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// Each command carries ITS context's metadata: a worker kept warm across jobs must never send a stale run id.
func TestVenueSendsBuildMetadataPerCommand(t *testing.T) {
	t.Parallel()

	runner := newLocalRunner(t, localWorker(t, t.TempDir()))

	for _, id := range []string{"RUN-A", "RUN-B"} {
		ctx := shell.WithBuildMetadata(context.Background(), shell.BuildMetadata{RunID: id})

		out, err := runner.RunCapture(ctx, `printf %s "$STEPS_RUN_ID"`)
		if err != nil {
			t.Fatalf("RunCapture: %v", err)
		}

		if strings.TrimSpace(string(out)) != id {
			t.Errorf("command saw %q, want %q", out, id)
		}
	}

	out, err := runner.RunCapture(context.Background(), `printf %s "${STEPS_RUN_ID-none}"`)
	if err != nil || string(out) != "none" {
		t.Errorf("a context with no metadata saw %q, %v; want none", out, err)
	}
}
