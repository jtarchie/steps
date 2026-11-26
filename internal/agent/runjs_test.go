package agent

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dop251/goja"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// scriptEnv is a tool env whose scripts call registry directly, with no budget or record: what run_js does with a call, without a conversation around it.
func scriptEnv(registry map[string]toolImpl) toolEnv {
	return toolEnv{dir: ".", script: scriptTools{
		names: scriptableTools(registry),
		call: func(ctx context.Context, env toolEnv, name string, args map[string]any) map[string]any {
			return registry[name](ctx, args, env)
		},
	}}
}

func runScript(t *testing.T, registry map[string]toolImpl, code string) map[string]any {
	t.Helper()

	return execRunJS(t.Context(), map[string]any{runJSCodeArg: code}, scriptEnv(registry))
}

func echoTool(_ context.Context, args map[string]any, _ toolEnv) map[string]any {
	return map[string]any{"got": args, "items": []string{"a", "b"}}
}

func TestRunJSHandsToolsJSONAndReturnsTheLastExpression(t *testing.T) {
	t.Parallel()

	var seen any

	registry := map[string]toolImpl{
		"echo": func(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
			seen = args["n"]

			return echoTool(ctx, args, env)
		},
		"get-thing": echoTool,
	}

	got := runScript(t, registry, `
const r = echo({n: 3});
console.log("items", r.items);
({n: r.got.n, real: Array.isArray(r.items), odd: tools["get-thing"]({}).items.length})
`)

	if _, isFloat := seen.(float64); !isFloat {
		t.Errorf("the tool received n as %T, want float64 as a model's own call sends it", seen)
	}

	result, _ := got["result"].(map[string]any)
	if result["n"] != float64(3) || result["real"] != true || result["odd"] != float64(2) {
		t.Errorf("result = %v, want n:3 real:true odd:2 (full: %v)", result, got)
	}

	if got["logs"] != "items [\"a\",\"b\"]\n" {
		t.Errorf("logs = %q", got["logs"])
	}
}

func TestRunJSReportsAScriptErrorAsData(t *testing.T) {
	t.Parallel()

	for name, code := range map[string]string{
		"syntax":  `const = 1`,
		"thrown":  `console.log("before"); throw new Error("boom")`,
		"missing": `nope({})`,
		"empty":   `  `,
		"deep":    `function f() { return f() } f()`,
		"args":    `echo("not an object")`,
		"nan":     `echo(NaN)`,
		// The three below run the script's code after the script has returned, where a throw has no frame to land in.
		"getter":   `({get x() { throw new Error("boom") }})`,
		"tostring": `throw {toString() { throw new Error("inner") }}`,
		"rejected": `(async () => { throw {toString() { throw new Error("inner") }} })()`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := runScript(t, map[string]toolImpl{"echo": echoTool}, code)

			message, _ := got[resultErrorKey].(string)
			if !strings.HasPrefix(message, "run_js: ") {
				t.Fatalf("result = %v, want an error naming run_js", got)
			}

			if name == "thrown" && got["logs"] != "before\n" {
				t.Errorf("logs before the throw were dropped: %v", got)
			}
		})
	}
}

func TestRunJSCannotCallItselfOrEndTheConversation(t *testing.T) {
	t.Parallel()

	registry := map[string]toolImpl{
		config.RunJSBuiltinName:   execRunJS,
		config.AskUserBuiltinName: echoTool,
		verdictToolName:           echoTool,
		"echo":                    echoTool,
		"JSON":                    echoTool,
	}

	got := runScript(t, registry, `[typeof run_js, typeof ask_user, typeof verdict, typeof echo, typeof JSON.stringify, typeof tools.JSON, Object.keys(tools).length]`)

	want := []any{"undefined", "undefined", "undefined", "function", "function", "function", float64(2)}

	result, _ := got["result"].([]any)
	if len(result) != len(want) {
		t.Fatalf("result = %v", got)
	}

	for i := range want {
		if result[i] != want[i] {
			t.Errorf("result[%d] = %v, want %v", i, result[i], want[i])
		}
	}
}

func TestRunJSReadsAnAsyncScriptsSettledValue(t *testing.T) {
	t.Parallel()

	registry := map[string]toolImpl{"echo": echoTool}

	got := runScript(t, registry, `(async () => { const r = await echo({n: 1}); return r.got.n })()`)
	if got["result"] != float64(1) {
		t.Errorf("fulfilled: %v", got)
	}

	got = runScript(t, registry, `(async () => { throw new Error("late") })()`)
	if message, _ := got[resultErrorKey].(string); !strings.Contains(message, "late") {
		t.Errorf("rejected: %v", got)
	}

	got = runScript(t, registry, `new Promise(() => {})`)
	if message, _ := got[resultErrorKey].(string); !strings.Contains(message, "never settles") {
		t.Errorf("pending: %v", got)
	}

	got = runScript(t, registry, `var x = 1;`)
	if value, present := got["result"]; !present || value != nil {
		t.Errorf("a script ending on a statement: %v", got)
	}
}

func TestRunJSBoundsWhatItReturns(t *testing.T) {
	t.Parallel()

	got := runScript(t, nil, `for (let i = 0; i < 100000; i++) console.log("x".repeat(100)); "y".repeat(`+"1e6"+`)`)

	if got["truncated"] != true {
		t.Errorf("an oversized result was returned whole: %d bytes", len(got["result"].(string)))
	}

	if text, _ := got["result"].(string); len(text) > 2*maxToolOutputBytes {
		t.Errorf("result is %d bytes, over the %d limit", len(text), maxToolOutputBytes)
	}

	if logs, _ := got["logs"].(string); len(logs) > 2*maxToolOutputBytes {
		t.Errorf("logs are %d bytes, over the %d limit", len(logs), maxToolOutputBytes)
	}

	for name, code := range map[string]string{
		"thrown":   `throw "y".repeat(1e6)`,
		"rejected": `(async () => { throw "y".repeat(1e6) })()`,
	} {
		failed := runScript(t, nil, code)
		if message, _ := failed[resultErrorKey].(string); message == "" || len(message) > 2*maxToolOutputBytes {
			t.Errorf("%s: the error is %d bytes, want it held to the %d limit", name, len(message), maxToolOutputBytes)
		}
	}
}

// The output cap alone would let a script hold everything it printed until it finished.
func TestRunJSStopsHoldingLogsPastTheLimit(t *testing.T) {
	t.Parallel()

	vm := goja.New()
	script := jsScript{vm: vm, limit: 64}

	for range 1000 {
		script.log(goja.FunctionCall{Arguments: []goja.Value{vm.ToValue("0123456789abcdef")}})
	}

	if held := script.logs.Len(); held > 128 {
		t.Errorf("holding %d bytes of logs under a limit of 64", held)
	}
}

// TestRunJSTimeoutStopsAScriptThatCallsNothing goes through buildAgentTools, because the deadline is bound there and a script that never calls a tool never reads its context.
func TestRunJSTimeoutStopsAScriptThatCallsNothing(t *testing.T) {
	t.Parallel()

	tools, closers, err := buildAgentTools(t.Context(), nil, []config.ToolSpec{{Builtin: config.RunJSBuiltinName, Timeout: "50ms"}}, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	t.Cleanup(func() { closeAll(closers) })

	done := make(chan map[string]any, 1)

	go func() {
		done <- tools.registry[config.RunJSBuiltinName](context.WithoutCancel(t.Context()), map[string]any{runJSCodeArg: `while (true) {}`}, toolEnv{dir: "."})
	}()

	select {
	case got := <-done:
		if message, _ := got[resultErrorKey].(string); !strings.Contains(message, "timed out after 50ms") {
			t.Errorf("result = %v, want the tool's own timeout named", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a spinning script outlived its timeout")
	}
}

func TestRunJSHasADeadlineEvenWhenItsGrantSetsNone(t *testing.T) {
	t.Parallel()

	if got := declaredTimeout(config.ToolSpec{Builtin: config.RunJSBuiltinName}); got != runJSDefaultTimeout {
		t.Errorf("run_js with no timeout: is bound to %q, want %q", got, runJSDefaultTimeout)
	}

	if got := declaredTimeout(config.ToolSpec{Builtin: config.RunJSBuiltinName, Timeout: "5m"}); got != "5m" {
		t.Errorf("run_js with timeout: 5m is bound to %q", got)
	}

	if got := declaredTimeout(config.ToolSpec{Builtin: config.RunShellBuiltinName}); got != "" {
		t.Errorf("run_shell with no timeout: is bound to %q, want none", got)
	}
}

func TestRunJSStopsCallingToolsOnceCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())

	var calls atomic.Int64

	registry := map[string]toolImpl{"stop": func(context.Context, map[string]any, toolEnv) map[string]any {
		calls.Add(1)
		cancel()

		return map[string]any{}
	}}

	got := execRunJS(ctx, map[string]any{runJSCodeArg: `try { stop({}); stop({}) } catch (e) {} stop({})`}, scriptEnv(registry))

	if calls.Load() != 1 {
		t.Errorf("the tool ran %d times after the step was cancelled, want 1", calls.Load())
	}

	if _, failed := got[resultErrorKey]; !failed {
		t.Errorf("a cancelled script reported success: %v", got)
	}
}

// TestCLIBridgeHoldsAScriptsCallsToTheBudget crosses the seam the hosted e2e cannot: a CLI child's run_js reaches the other tools through the bridge's own budget and capture, not the turn loop's.
func TestCLIBridgeHoldsAScriptsCallsToTheBudget(t *testing.T) {
	t.Parallel()

	var invocations atomic.Int64

	decls := []*genai.FunctionDeclaration{runJSTool().decl, {
		Name: "post_review", Description: "post", Parameters: &genai.Schema{Type: genai.TypeObject},
	}}

	registry := map[string]toolImpl{
		config.RunJSBuiltinName: execRunJS,
		"post_review": func(context.Context, map[string]any, toolEnv) map[string]any {
			invocations.Add(1)

			return map[string]any{"exit_code": 0}
		},
	}

	conv := bridgeConversation(decls, registry, nil)
	conv.tools.maxCalls = map[string]int{"post_review": 1}

	bridge, err := newCLIBridge(t.Context(), conv, nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	result, err := dialBridge(t, bridge).CallTool(t.Context(), &sdkmcp.CallToolParams{
		Name:      config.RunJSBuiltinName,
		Arguments: map[string]any{runJSCodeArg: `[post_review({}), post_review({})].map(r => r.error || "ok")`},
	})
	if err != nil {
		t.Fatalf("run_js: %v", err)
	}

	if text := bridgeText(t, result); !strings.Contains(text, `"ok"`) || !strings.Contains(text, "call budget (1) exhausted") {
		t.Errorf("result = %s, want one call made and one refused", text)
	}

	if invocations.Load() != 1 {
		t.Errorf("the tool impl ran %d times under a budget of 1", invocations.Load())
	}

	_, _, _, calls := bridge.observed()

	names := make([]string, 0, len(calls))
	for _, call := range calls {
		names = append(names, call.name)
	}

	if got := strings.Join(names, ","); got != "post_review,post_review,run_js" {
		t.Errorf("captured calls = %s, want both of the script's calls, the refused one too, and then the script", got)
	}
}

// The stream names the model's own calls and never a script's, so a script's read_file is not one of the stream's read_files under another count: it is kept, with its own arguments, behind the run_js that made it.
func TestMergeCLITrajectoryFilesAScriptsCallsBehindItsRunJS(t *testing.T) {
	t.Parallel()

	streamed := []recordedToolCall{
		{name: "run_js", ok: true},
		{name: "read_file", args: map[string]any{"path": "direct"}, ok: true},
		{name: "run_js", ok: true},
	}

	bridged := []recordedToolCall{
		{name: "read_file", args: map[string]any{"path": "scripted"}, ok: true, scripted: true},
		{name: "run_js", ok: true},
		{name: "read_file", args: map[string]any{"path": "direct"}, ok: true},
		{name: "post_review", ok: true, scripted: true},
		{name: "run_js", ok: true},
	}

	got := make([]string, 0, len(bridged))

	for _, call := range mergeCLITrajectory(streamed, bridged) {
		entry := call.name
		if path, _ := call.args["path"].(string); path != "" {
			entry += ":" + path
		}

		got = append(got, entry)
	}

	if want := "run_js,read_file:scripted,read_file:direct,run_js,post_review"; strings.Join(got, ",") != want {
		t.Errorf("merged = %s, want %s", strings.Join(got, ","), want)
	}
}

// A script's call is filed behind the run_js that made it, ahead of a call the model made later in the same turn, and what it returns names the spill directory as the model knows it.
func TestHostedScriptCallsAreFiledBehindTheirRunJS(t *testing.T) {
	t.Parallel()

	registry := map[string]toolImpl{
		"spill": func(_ context.Context, _ map[string]any, env toolEnv) map[string]any {
			return map[string]any{"stdout": "saved to " + env.spillDir + "/output-1.txt"}
		},
	}

	conv := agentConversation{tools: agentTools{registry: registry, required: map[string]bool{}}}
	state := resumeCheckpoint{
		satisfied:  map[string]bool{},
		callCounts: map[string]int{},
		trajectory: []recordedToolCall{{name: "earlier"}, {name: "run_js"}, {name: "post_review"}, {name: "run_js"}},
	}
	turnStart := 1
	script := conv.scriptTools(&state, &turnStart)

	env := toolEnv{dir: ".", spillDir: "/host/step/.steps-spill", spillShown: "/work/.steps-spill"}

	first := script.call(t.Context(), env, "spill", nil)
	script.call(t.Context(), env, "spill", nil)

	env.turnCall = 2
	script.call(t.Context(), env, "spill", nil)

	names := make([]string, 0, len(state.trajectory))
	for _, call := range state.trajectory {
		names = append(names, call.name)
	}

	if got := strings.Join(names, ","); got != "earlier,run_js,spill,spill,post_review,run_js,spill" {
		t.Errorf("trajectory = %s, want each script's calls directly behind its own run_js", got)
	}

	if got, _ := first["stdout"].(string); got != "saved to /work/.steps-spill/output-1.txt" {
		t.Errorf("the script was told %q, want the spill directory under the name the model knows", got)
	}

	parts := []*genai.Part{
		{FunctionResponse: &genai.FunctionResponse{Name: "run_js", Response: map[string]any{}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "post_review", Response: map[string]any{"error": "no"}}},
		{FunctionResponse: &genai.FunctionResponse{Name: "run_js", Response: map[string]any{}}},
	}

	markTrajectoryResults(state.trajectory[turnStart:], parts)

	marks := make([]bool, 0, len(state.trajectory))
	for _, call := range state.trajectory {
		marks = append(marks, call.ok)
	}

	if want := []bool{false, true, true, true, false, true, true}; !slices.Equal(marks, want) {
		t.Errorf("ok marks = %v, want %v: each of the turn's results on the model's own call, past the scripted ones", marks, want)
	}
}

// TestRunJSDescriptionNamesWhatThisAgentsScriptCanCall holds the description to the grant: a CLI-backed model sees its tools under a prefix a script does not define, so the exact names, and the deadline the grant set, are told to it rather than left to "the same name".
func TestRunJSDescriptionNamesWhatThisAgentsScriptCanCall(t *testing.T) {
	t.Parallel()

	describe := func(specs ...config.ToolSpec) string {
		tools, closers, err := buildAgentTools(t.Context(), nil, specs, "")
		if err != nil {
			t.Fatalf("buildAgentTools: %v", err)
		}

		defer closeAll(closers)

		for _, decl := range tools.decls.FunctionDeclarations {
			if decl.Name == config.RunJSBuiltinName {
				return decl.Description
			}
		}

		t.Fatal("run_js was not declared")

		return ""
	}

	granted := describe(
		config.ToolSpec{Builtin: config.WriteFileBuiltinName},
		config.ToolSpec{Builtin: config.RunJSBuiltinName, Timeout: "2m"},
		config.ToolSpec{Builtin: config.AskUserBuiltinName},
		config.ToolSpec{Builtin: config.ReadFileBuiltinName},
	)

	for _, want := range []string{"Callable from a script here: read_file, write_file.", "mcp__", "Time limit: 2m."} {
		if !strings.Contains(granted, want) {
			t.Errorf("description lacks %q:\n%s", want, granted)
		}
	}

	alone := describe(config.ToolSpec{Builtin: config.RunJSBuiltinName})

	for _, want := range []string{"No other tool is granted here, so a script can call nothing.", "Time limit: " + runJSDefaultTimeout + "."} {
		if !strings.Contains(alone, want) {
			t.Errorf("description lacks %q:\n%s", want, alone)
		}
	}
}

// TestEveryBuiltinTheLoaderAcceptsIsImplemented holds config's name list to this package's catalogue in both directions: a name only config knows fails when its step starts, and one only this package knows is refused at load.
func TestEveryBuiltinTheLoaderAcceptsIsImplemented(t *testing.T) {
	t.Parallel()

	catalogue := builtinAgentTools("")

	implemented := make([]string, 0, len(catalogue)+1)
	implemented = append(implemented, config.AskUserBuiltinName)

	for name := range catalogue {
		implemented = append(implemented, name)
	}

	accepted := config.BuiltinToolNames()

	slices.Sort(implemented)
	slices.Sort(accepted)

	if !slices.Equal(implemented, accepted) {
		t.Errorf("builtins implemented = %v, but the loader accepts %v", implemented, accepted)
	}
}
