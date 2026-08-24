package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

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

// TestWithToolTimeoutKeepsTheImplsOwnDiagnostic: the deadline is the less
// useful half of what happened. web_fetch names the URL that hung, a
// sub-agent names the child's failure — replacing that with "timed out"
// would leave an operator reading the transcript with no idea what was being
// waited on.
func TestWithToolTimeoutKeepsTheImplsOwnDiagnostic(t *testing.T) {
	t.Parallel()

	slow := func(ctx context.Context, _ map[string]any, _ toolEnv) map[string]any {
		<-ctx.Done()

		return map[string]any{"error": `web_fetch: Get "https://api.example.com/x": context deadline exceeded`}
	}

	result := withToolTimeout("web_fetch", 20*time.Millisecond, slow)(context.Background(), nil, toolEnv{})

	msg, _ := result["error"].(string)
	if !strings.Contains(msg, "timed out after 20ms") || !strings.Contains(msg, "https://api.example.com/x") {
		t.Errorf("error = %q, want both the deadline and the impl's own error", msg)
	}
}

// TestWithToolTimeoutNeverRelabelsASuccess is the rule that keeps the
// context-ignoring built-ins honest. read_file/list_dir/search_files/
// write_file/edit_file take no context, so a slow filesystem can carry one
// past its deadline and it still returns having done the whole job. Stamping
// an error on that would be a lie the machinery acts on: requiredCallSucceeded
// would flip to false, a required: tool would be force-called again, the CLI
// bridge would report IsError, and a model would be told to redo work that
// is already on disk.
func TestWithToolTimeoutNeverRelabelsASuccess(t *testing.T) {
	t.Parallel()

	ignoresContext := func(context.Context, map[string]any, toolEnv) map[string]any {
		time.Sleep(30 * time.Millisecond)

		return map[string]any{"path": "notes.md", "bytes_written": 12}
	}

	result := withToolTimeout("write_file", 5*time.Millisecond, ignoresContext)(context.Background(), nil, toolEnv{})
	if result["error"] != nil {
		t.Errorf("error = %v, want none: the call finished its work", result["error"])
	}

	if !requiredCallSucceeded(result) {
		t.Error("an over-deadline call that did its whole job must still read as success")
	}
}

// TestWithToolTimeoutLeavesAZeroExitAlone is the same rule where it is least
// obvious. requiredCallSucceeded reads exit_code BEFORE error, so a
// shell-backed call that finished cleanly on the deadline boundary would
// otherwise carry a "timed out" error the success verdict flatly contradicts
// — data the model has no way to reconcile.
func TestWithToolTimeoutLeavesAZeroExitAlone(t *testing.T) {
	t.Parallel()

	raced := func(ctx context.Context, _ map[string]any, _ toolEnv) map[string]any {
		<-ctx.Done()

		return map[string]any{"stdout": "done", "stderr": "", "exit_code": 0}
	}

	result := withToolTimeout("watch", 5*time.Millisecond, raced)(context.Background(), nil, toolEnv{})
	if result["error"] != nil {
		t.Errorf("error = %v, want none alongside exit_code 0", result["error"])
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

// TestBuildAgentToolsRejectsAnUnbindableToolTimeout: LoadConfig validates the
// duration, but buildAgentTools is reachable without it (RunFix, tests), and
// a deadline that silently did not bind is worse than a refused one. "0" is
// refused for the same reason it is at load — as an ambiguity, not as a way
// to spell "unbounded", which is what omitting the field already means.
func TestBuildAgentToolsRejectsAnUnbindableToolTimeout(t *testing.T) {
	t.Parallel()

	for _, timeout := range []string{"soon", "0", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			t.Parallel()

			specs := []config.ToolSpec{{Name: "watch", Run: "true", Timeout: timeout}}

			_, closers, err := buildAgentTools(t.Context(), nil, specs, "")
			closeAll(closers)

			if err == nil {
				t.Fatalf("buildAgentTools accepted timeout %q", timeout)
			}
		})
	}
}

// TestBuildAgentToolsClosesAConnectionABadTimeoutRejects covers the ordering
// hazard the binding introduced: an MCP grant is already CONNECTED by the
// time its timeout: is parsed, so a spec that resolves and then fails to bind
// must still surrender its closer to the caller's cleanup. Collecting the
// closer after the bind would leave a live streamable-HTTP client (and, for a
// stdio server, a subprocess) behind on a step that never started — the exact
// thing buildAgentTools' own doc comment promises cannot happen.
//
// A closed client sends the streamable-HTTP session-termination DELETE, so
// counting that is a direct observation of the connection being released,
// rather than an inference from goroutine census.
func TestBuildAgentToolsClosesAConnectionABadTimeoutRejects(t *testing.T) {
	t.Parallel()

	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "test", Version: "v0"}, nil)
	server.AddTool(&sdkmcp.Tool{
		Name:        "search_issues",
		Description: "Search issues.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		return &sdkmcp.CallToolResult{}, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, nil)

	var (
		mu         sync.Mutex
		terminated int
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			terminated++
			mu.Unlock()
		}

		handler.ServeHTTP(w, r)
	}))

	// Registered in this order so CloseClientConnections runs FIRST (cleanups
	// are LIFO): without it, a regression strands a connection the httptest
	// server's own Close then blocks on, turning a failing test into a hung
	// package.
	t.Cleanup(ts.Close)
	t.Cleanup(ts.CloseClientConnections)

	cfg := &config.Config{MCPServers: []config.MCPServer{{Name: "test", Endpoint: ts.URL}}}

	_, closers, err := buildAgentTools(t.Context(), cfg, []config.ToolSpec{{MCP: "test", Timeout: "soon"}}, "")
	closeAll(closers)

	if err == nil {
		t.Fatal("buildAgentTools accepted an unparseable timeout on an mcp grant")
	}

	mu.Lock()
	defer mu.Unlock()

	if terminated == 0 {
		t.Error("the connected mcp client was never closed: a resolved spec that fails to bind leaks it")
	}
}
