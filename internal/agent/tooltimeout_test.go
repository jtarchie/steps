package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// TestWithToolTimeoutReportsExpiryAsData pins the wrapper's whole contract:
// the impl gets a context that expires at the tool's own deadline, and the
// expiry comes back as tool-result data rather than a Go error, keeping
// whatever the impl produced on its way out.
func TestWithToolTimeoutReportsExpiryAsData(t *testing.T) {
	t.Parallel()

	slow := func(ctx context.Context, _ map[string]any, _ toolEnv) map[string]any {
		<-ctx.Done()

		return map[string]any{"stdout": "partial", "exit_code": 1}
	}

	result := withToolTimeout("watch", 20*time.Millisecond, slow)(context.Background(), nil, toolEnv{})

	msg, _ := result["error"].(string)
	if !strings.Contains(msg, "watch: timed out after 20ms") {
		t.Errorf("error = %q, want it to name the tool and its deadline", msg)
	}

	if got, _ := result["stdout"].(string); got != "partial" {
		t.Errorf("stdout = %q, want the impl's partial output preserved alongside the error", got)
	}
}

// TestWithToolTimeoutLeavesAFastCallAlone: a call that finishes inside its
// deadline is untouched — no error key invented, nothing rewritten.
func TestWithToolTimeoutLeavesAFastCallAlone(t *testing.T) {
	t.Parallel()

	quick := func(context.Context, map[string]any, toolEnv) map[string]any {
		return map[string]any{"stdout": "done", "exit_code": 0}
	}

	result := withToolTimeout("watch", time.Minute, quick)(context.Background(), nil, toolEnv{})
	if result["error"] != nil {
		t.Errorf("error = %v, want none", result["error"])
	}
}

// TestWithToolTimeoutDoesNotClaimAParentCancel is the distinction the
// wrapper exists to draw. When the STEP is cancelled — its own timeout:, or
// SIGINT — the child context is done too, and reporting that as the tool's
// deadline would misname why the run ended and hand the model a fiction to
// react to.
func TestWithToolTimeoutDoesNotClaimAParentCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	waits := func(callCtx context.Context, _ map[string]any, _ toolEnv) map[string]any {
		<-callCtx.Done()

		return map[string]any{"error": "command failed: signal: killed"}
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	defer cancel()

	result := withToolTimeout("watch", time.Hour, waits)(ctx, nil, toolEnv{})

	if msg, _ := result["error"].(string); strings.Contains(msg, "timed out") {
		t.Errorf("error = %q, want the impl's own cancellation error left alone", msg)
	}
}

// TestBuildAgentToolsBindsTheTimeoutToTheImpl proves the binding happens at
// BUILD time, on the impl itself — which is what makes the deadline hold on
// the CLI-delegated path too, where the bridge hands the child these same
// impls over MCP and never passes through the conversation loop.
func TestBuildAgentToolsBindsTheTimeoutToTheImpl(t *testing.T) {
	t.Parallel()

	specs := []config.ToolSpec{{
		Name:        "watch",
		Description: "hangs",
		Run:         "sleep 30",
		Timeout:     "50ms",
	}}

	tools, closers, err := buildAgentTools(t.Context(), nil, specs, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	defer closeAll(closers)

	start := time.Now()
	result := tools.registry["watch"](t.Context(), map[string]any{}, testEnv(t.TempDir()))

	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("call took %s; the command outlived its own deadline", elapsed)
	}

	if msg, _ := result["error"].(string); !strings.Contains(msg, "timed out after 50ms") {
		t.Errorf("error = %q, want the timeout report", msg)
	}
}

// TestBuildAgentToolsRejectsAnUnparseableToolTimeout: LoadConfig validates
// the duration, but buildAgentTools is reachable without it (RunFix, tests),
// and a deadline that silently did not bind is worse than a refused one.
func TestBuildAgentToolsRejectsAnUnparseableToolTimeout(t *testing.T) {
	t.Parallel()

	specs := []config.ToolSpec{{Name: "watch", Run: "true", Timeout: "soon"}}

	_, closers, err := buildAgentTools(t.Context(), nil, specs, "")
	closeAll(closers)

	if err == nil {
		t.Fatal("buildAgentTools accepted an unparseable tool timeout")
	}
}
