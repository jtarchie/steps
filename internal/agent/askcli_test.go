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
)

// TestAskUserIsNeverNativeToACLI is the whole of the bridge wiring, asserted
// rather than assumed: not adding ask_user to a runtime's natives table is
// what makes it bridged, which is in turn what makes the question land in THIS
// process — where the questions row, the memo and the responder ladder live.
//
// Mapping it onto a CLI's own ask-the-user tool would mean the CLI path had a
// different feature with the same name: the answer would land in the child's
// transcript, and nothing here would ever see it.
func TestAskUserIsNeverNativeToACLI(t *testing.T) {
	t.Parallel()

	for cli, runtime := range cliRuntimes {
		if native, claimed := runtime.natives[config.AskUserBuiltinName]; claimed {
			t.Errorf("cli %q claims %s is native (as %q); it must be bridged so the answer reaches this process",
				cli, config.AskUserBuiltinName, native)
		}

		if !config.BuiltinIsNeverNativeToCLI(config.AskUserBuiltinName) {
			t.Errorf("config and this package disagree about whether %s is native to cli %q",
				config.AskUserBuiltinName, cli)
		}
	}
}

// TestCLIBridgeEnforcesTheQuestionBudget: max_questions: is a config.Step dial
// rather than a ToolSpec guard, so it slips past the load-time check that
// refuses required:/max_calls:/args: for a cli source — and would land in the
// same trap those refusals exist to prevent, a promise nothing applies. The
// bridge handler is the only place on this path that sees every ask.
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

	bridge, err := newCLIBridge(t.Context(), conv, nil, reachHost)
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

	want := (45*time.Minute + cliToolTimeoutMargin).Milliseconds()
	if got := env[0]; got != cliMCPToolTimeoutEnv+"="+strconv.FormatInt(want, 10) {
		t.Errorf("cliToolTimeoutEnv = %q, want %s=%d (the declared wait plus a margin)", got, cliMCPToolTimeoutEnv, want)
	}

	// A step that cannot ask needs no widening at all.
	if env := cliToolTimeoutEnv(config.ResolvedInvocation{}); env != nil {
		t.Errorf("a step without an ask_user grant set %v", env)
	}

	// And an operator's own value is theirs, not this function's to overrule.
	t.Setenv(cliMCPToolTimeoutEnv, "1000")

	if env := cliToolTimeoutEnv(ri); env != nil {
		t.Errorf("an explicitly configured %s was overruled by %v", cliMCPToolTimeoutEnv, env)
	}
}
