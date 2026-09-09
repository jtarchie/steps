package agent

// What a CLI-backed agent's ask_user grant costs, and what it gets for free.

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// TestCLIBridgeEnforcesTheQuestionBudget: max_questions: is a config.Step dial
// rather than a ToolSpec guard, so it has never gone through the load-time
// tool-guard check at all (see internal/config's checkCLIAgentTools, which
// since issue #100 no longer refuses required:/max_calls:/args: either — the
// bridge, or the exit check below it, is where all of them bind). The bridge
// handler is the only place on this path that sees every ask.
func TestCLIBridgeEnforcesTheQuestionBudget(t *testing.T) {
	t.Parallel()

	var asked atomic.Int64

	decls := []*genai.FunctionDeclaration{{
		Name: config.AskUserBuiltinName, Description: "ask", Parameters: &genai.Schema{Type: genai.TypeObject},
	}}

	registry := map[string]toolImpl{
		config.AskUserBuiltinName: func(context.Context, map[string]any, toolEnv) map[string]any {
			asked.Add(1)

			return map[string]any{"exit_code": 0, "answer": "minor"}
		},
	}

	conv := bridgeConversation(decls, registry, nil)
	conv.tools.maxCalls = map[string]int{config.AskUserBuiltinName: 2}

	bridge, err := newCLIBridge(t.Context(), conv, nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	session := dialBridge(t, bridge)

	for call := 1; call <= 3; call++ {
		result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: config.AskUserBuiltinName})
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}

		overBudget := call > 2
		if result.IsError != overBudget {
			t.Errorf("call %d: IsError = %v, want %v", call, result.IsError, overBudget)
		}

		if overBudget && !strings.Contains(bridgeText(t, result), "budget") {
			t.Errorf("call %d: the refusal does not name the exhausted budget: %s", call, bridgeText(t, result))
		}
	}

	// Rejected BEFORE the impl runs: the point of this budget is bounding the
	// side effect, which here is interrupting somebody.
	if asked.Load() != 2 {
		t.Errorf("the ask_user impl ran %d times under a budget of 2", asked.Load())
	}
}

// bridgeText is the text content of a bridged call's result.
func bridgeText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) == 0 {
		return ""
	}

	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("bridged result content = %T, want text", result.Content[0])
	}

	return text.Text
}

// TestCLIToolTimeoutCoversAParkedQuestion: a bridged call blocks until it
// returns, and the binding constraint is the CHILD's own tool-call deadline,
// which is the CLI's default and not ours. Without widening it, a parked
// question dies at whatever the CLI decided rather than at the deadline the
// pipeline declared — and the model is told its question failed while a person
// is still looking at it.
func TestCLIToolTimeoutCoversAParkedQuestion(t *testing.T) {
	ri := config.ResolvedInvocation{ToolSpecs: []config.ToolSpec{
		{Builtin: config.AskUserBuiltinName, Timeout: "45m"},
	}}

	env := cliToolTimeoutEnv(ri)
	if len(env) != 1 {
		t.Fatalf("cliToolTimeoutEnv = %v, want one entry", env)
	}

	// max(the default step timeout, the ask_user wait + a margin) + a margin.
	want := (45*time.Minute + 2*cliToolTimeoutMargin).Milliseconds()
	if got := env[0]; got != cliMCPToolTimeoutEnv+"="+strconv.FormatInt(want, 10) {
		t.Errorf("cliToolTimeoutEnv = %q, want %s=%d (the declared wait plus two margins)", got, cliMCPToolTimeoutEnv, want)
	}

	// An operator's own value is FORWARDED, not deferred to. The distinction
	// is the whole bug: MCP_TOOL_TIMEOUT is not on shell.HostEnv's allowlist,
	// so leaving it out of cmd.Env means the child never sees it — and the
	// case where somebody had thought about the deadline was the one case
	// where none was applied at all.
	t.Setenv(cliMCPToolTimeoutEnv, "1000")

	forwarded := cliToolTimeoutEnv(ri)
	if len(forwarded) != 1 || forwarded[0] != cliMCPToolTimeoutEnv+"=1000" {
		t.Errorf("cliToolTimeoutEnv = %v, want the operator's own value forwarded to the child", forwarded)
	}

	// The allowlist is why: without an explicit entry, nothing carries it.
	for _, kv := range shell.HostEnv() {
		if strings.HasPrefix(kv, cliMCPToolTimeoutEnv+"=") {
			t.Errorf("%s is on the host env allowlist; this test's premise is stale", cliMCPToolTimeoutEnv)
		}
	}
}

// TestCLIToolTimeoutCoversEveryToolNow is R1: after issue #100's flip, EVERY
// tool call is a bridged MCP call, so a step with no ask_user grant at all
// still needs MCP_TOOL_TIMEOUT set — an ordinary run_shell must not die at
// the CLI's own un-widened default. Before the flip this returned nil.
func TestCLIToolTimeoutCoversEveryToolNow(t *testing.T) {
	env := cliToolTimeoutEnv(config.ResolvedInvocation{})
	if len(env) != 1 {
		t.Fatalf("cliToolTimeoutEnv = %v, want one entry even with no ask_user grant", env)
	}

	// The bound is the LARGER of the step's own default timeout and
	// askUserWait's own default (even absent a grant — see cliToolTimeoutEnv),
	// plus a margin over whichever won.
	base := agentStepTimeout
	if ask := defaultAskUserWait + cliToolTimeoutMargin; ask > base {
		base = ask
	}

	want := (base + cliToolTimeoutMargin).Milliseconds()
	if got := env[0]; got != cliMCPToolTimeoutEnv+"="+strconv.FormatInt(want, 10) {
		t.Errorf("cliToolTimeoutEnv = %q, want %s=%d (the step's own default timeout plus two margins)", got, cliMCPToolTimeoutEnv, want)
	}

	// An explicit timeout: 0 (no deadline of its own) still needs a number:
	// MCP_TOOL_TIMEOUT cannot be left unset, so it gets a generous ceiling
	// rather than nothing — the real bound is then the job's.
	uncapped := cliToolTimeoutEnv(config.ResolvedInvocation{Timeout: "0"})
	if got := uncapped[0]; got != cliMCPToolTimeoutEnv+"="+strconv.FormatInt((cliUnboundedToolTimeout+cliToolTimeoutMargin).Milliseconds(), 10) {
		t.Errorf("cliToolTimeoutEnv(timeout: 0) = %q, want the unbounded ceiling plus a margin", got)
	}
}
