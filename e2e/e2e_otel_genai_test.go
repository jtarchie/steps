package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// leadSummarizerPipeline is a lead agent that delegates once to a summarizer sub-agent, both against fakeURL.
func leadSummarizerPipeline(fakeURL string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  description: Condenses a request to a sentence.
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
- name: lead
  source: { endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - agent: summarizer
    description: Summarize; pass the text in request.

jobs:
- name: digest
  plan:
  - agent: lead
    messages:
      - Have your summarizer condense the outage, then report.
`, fakeURL)
}

// routeLeadSummarizer answers the lead (the request offering the summarizer tool) with a delegation and then a final reply carrying a served model and cached tokens; the summarizer answers in one turn.
func routeLeadSummarizer(req capturedRequest) turn {
	if !slices.Contains(req.toolNames(), "summarizer") {
		return says("It was an outage.")
	}

	if len(req.toolResults()) > 0 {
		return turn{body: `{"id":"fake","object":"chat.completion","created":0,"model":"test-model-0613",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Summary: an outage."}}],
			"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":64}}}`}
	}

	return callsTool("summarizer", map[string]any{"request": "condense the outage"})
}

// TestOTelTracesAnAgentInvocationByTheGenAIConventions: an agent step is an invoke_agent span, a delegation is the sub-agent's own invoke_agent under the tool call that made it, and every GenAI span carries the agent that emitted it and the run as its conversation — the attributes Honeycomb's agent timeline groups by.
func TestOTelTracesAnAgentInvocationByTheGenAIConventions(t *testing.T) {
	collector := newOTLPCollector(t)

	fake := newRoutedFakeLLM(t, routeLeadSummarizer)
	mustRun(t, writePipeline(t, t.TempDir(), leadSummarizerPipeline(fake.URL)))

	spans, _ := collector.snapshot()
	runID, lead, helper := assertDelegationTree(t, spans)

	assertChatIdentities(t, spans, lead, helper, runID)

	_, final := leadTurns(t, spans, lead)

	if got := spanAttr(final, "gen_ai.response.model"); got != "test-model-0613" {
		t.Errorf("gen_ai.response.model = %q, want the model the provider said answered", got)
	}

	if got := intAttr(final, "gen_ai.usage.cache_read.input_tokens"); got != 64 {
		t.Errorf("gen_ai.usage.cache_read.input_tokens = %d, want 64", got)
	}

	if got := intAttr(lead, "gen_ai.usage.input_tokens"); got != 100 {
		t.Errorf("the lead's invoke_agent reports %d input tokens, want 100", got)
	}

	for _, span := range spans {
		if spanAttr(span, "gen_ai.input.messages") != "" || spanAttr(span, "gen_ai.tool.call.arguments") != "" {
			t.Errorf("%s exported content without OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", span.GetName())
		}
	}

	assertInvocationMetrics(t, collector)
}

// assertDelegationTree checks run → step → invoke_agent lead → execute_tool summarizer → invoke_agent summarizer, each in the run's conversation, and returns the run id and the two invocations.
func assertDelegationTree(t *testing.T, spans []*tracepb.Span) (string, *tracepb.Span, *tracepb.Span) {
	t.Helper()

	run := findSpan(t, spans, tracepb.Span_SPAN_KIND_SERVER, "RUN digest")
	step := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "agent lead")
	lead := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "invoke_agent lead")
	tool := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "execute_tool summarizer")
	helper := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "invoke_agent summarizer")

	assertUnder(t, run, step, lead)
	assertUnder(t, run, lead, tool)
	assertUnder(t, run, tool, helper)

	runID := spanAttr(run, "cicd.pipeline.run.id")
	if runID == "" {
		t.Fatal("the run span names no run id")
	}

	assertGenAIIdentity(t, lead, "lead", runID)
	assertGenAIIdentity(t, tool, "lead", runID)
	assertGenAIIdentity(t, helper, "summarizer", runID)

	if got := spanAttr(helper, "gen_ai.agent.description"); got != "Condenses a request to a sentence." {
		t.Errorf("the sub-agent's invoke_agent describes it as %q", got)
	}

	return runID, lead, helper
}

// assertChatIdentities checks each model call names the agent that made it, not the one that delegated.
func assertChatIdentities(t *testing.T, spans []*tracepb.Span, lead, helper *tracepb.Span, runID string) {
	t.Helper()

	leadChats := chatsUnder(spans, lead)
	helperChats := chatsUnder(spans, helper)

	if len(leadChats) != 2 || len(helperChats) != 1 {
		t.Fatalf("want 2 chat spans under the lead and 1 under the summarizer, got %d and %d", len(leadChats), len(helperChats))
	}

	for _, chat := range leadChats {
		assertGenAIIdentity(t, chat, "lead", runID)
	}

	assertGenAIIdentity(t, helperChats[0], "summarizer", runID)
}

// leadTurns is the lead's delegating turn (finish reason tool_call) and its final one (stop).
func leadTurns(t *testing.T, spans []*tracepb.Span, lead *tracepb.Span) (*tracepb.Span, *tracepb.Span) {
	t.Helper()

	reasons := map[string]*tracepb.Span{}
	for _, chat := range chatsUnder(spans, lead) {
		reasons[strings.Join(stringsAttr(chat, "gen_ai.response.finish_reasons"), ",")] = chat
	}

	delegation, delegated := reasons["tool_call"]
	final, stopped := reasons["stop"]

	if !delegated || !stopped {
		t.Fatalf("the lead's finish reasons = %v, want one tool_call turn and one stop turn", slices.Collect(maps.Keys(reasons)))
	}

	return delegation, final
}

func assertInvocationMetrics(t *testing.T, collector *otlpCollector) {
	t.Helper()

	lead := map[string]string{"gen_ai.agent.name": "lead"}
	summarizer := map[string]string{"gen_ai.agent.name": "summarizer"}

	if got := histogramCount(collector.latest("gen_ai.invoke_agent.duration"), lead); got != 1 {
		t.Errorf("gen_ai.invoke_agent.duration counted %d lead invocations, want 1", got)
	}

	inference := collector.latest("gen_ai.invoke_agent.inference_calls")
	if got := histogramSum(inference, lead); got != 2 {
		t.Errorf("the lead made %v inference calls, want 2 (its sub-agent's are its own)", got)
	}

	if got := histogramSum(inference, summarizer); got != 1 {
		t.Errorf("the summarizer made %v inference calls, want 1", got)
	}

	if got := histogramSum(collector.latest("gen_ai.invoke_agent.tool_calls"), lead); got != 1 {
		t.Errorf("the lead made %v tool calls, want 1", got)
	}

	if got := histogramCount(collector.latest("gen_ai.execute_tool.duration"), map[string]string{"gen_ai.tool.name": "summarizer", "gen_ai.agent.name": "lead"}); got != 1 {
		t.Errorf("gen_ai.execute_tool.duration counted %d summarizer calls, want 1", got)
	}
}

// TestOTelCapturesContentOnlyWhenAskedTo: OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT puts the conversation on the spans in the conventions' message shape — what was sent, what came back, the tool call's arguments and result.
func TestOTelCapturesContentOnlyWhenAskedTo(t *testing.T) {
	collector := newOTLPCollector(t)
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "true")

	fake := newRoutedFakeLLM(t, routeLeadSummarizer)
	mustRun(t, writePipeline(t, t.TempDir(), leadSummarizerPipeline(fake.URL)))

	spans, _ := collector.snapshot()
	lead := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "invoke_agent lead")
	tool := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "execute_tool summarizer")
	delegation, final := leadTurns(t, spans, lead)

	assertInputMessages(t, delegation, final)

	output := spanAttr(delegation, "gen_ai.output.messages")
	if !strings.Contains(output, `"type":"tool_call"`) || !strings.Contains(output, `"name":"summarizer"`) || !strings.Contains(output, `"finish_reason":"tool_call"`) {
		t.Errorf("gen_ai.output.messages = %s, want the summarizer tool call", output)
	}

	if got := spanAttr(tool, "gen_ai.tool.call.arguments"); got != `{"request":"condense the outage"}` {
		t.Errorf("gen_ai.tool.call.arguments = %s", got)
	}

	if got := spanAttr(tool, "gen_ai.tool.call.result"); !strings.Contains(got, "It was an outage.") {
		t.Errorf("gen_ai.tool.call.result = %s, want the sub-agent's answer", got)
	}
}

// assertInputMessages checks what each turn sent: the user's message and system prompt first, the tool's answer as a tool message after.
func assertInputMessages(t *testing.T, delegation, final *tracepb.Span) {
	t.Helper()

	var input []struct {
		Role  string           `json:"role"`
		Parts []map[string]any `json:"parts"`
	}

	err := json.Unmarshal([]byte(spanAttr(delegation, "gen_ai.input.messages")), &input)
	if err != nil {
		t.Fatalf("gen_ai.input.messages is not the conventions' JSON: %v", err)
	}

	if len(input) == 0 || input[len(input)-1].Role != "user" || input[len(input)-1].Parts[0]["content"] != "Have your summarizer condense the outage, then report." {
		t.Errorf("gen_ai.input.messages = %+v, want the user's message last", input)
	}

	if got := spanAttr(delegation, "gen_ai.system_instructions"); !strings.Contains(got, `"type":"text"`) {
		t.Errorf("gen_ai.system_instructions = %q, want the system prompt as text parts", got)
	}

	if got := spanAttr(final, "gen_ai.input.messages"); !strings.Contains(got, `{"role":"tool","parts":[{"id":"call_1","response":{"result":"It was an outage."},"type":"tool_call_response"}]}`) {
		t.Errorf("the final turn's gen_ai.input.messages = %s, want the summarizer's answer as a tool message", got)
	}
}

func assertGenAIIdentity(t *testing.T, span *tracepb.Span, agent, conversation string) {
	t.Helper()

	if got := spanAttr(span, "gen_ai.agent.name"); got != agent {
		t.Errorf("%s %x: gen_ai.agent.name = %q, want %q", span.GetName(), span.GetSpanId(), got, agent)
	}

	if got := spanAttr(span, "gen_ai.conversation.id"); got != conversation {
		t.Errorf("%s %x: gen_ai.conversation.id = %q, want %q", span.GetName(), span.GetSpanId(), got, conversation)
	}
}

func chatsUnder(spans []*tracepb.Span, parent *tracepb.Span) []*tracepb.Span {
	var found []*tracepb.Span

	for _, span := range spans {
		if span.GetName() == "chat test-model" && string(span.GetParentSpanId()) == string(parent.GetSpanId()) {
			found = append(found, span)
		}
	}

	return found
}

func stringsAttr(span *tracepb.Span, key string) []string {
	for _, attr := range span.GetAttributes() {
		if attr.GetKey() != key {
			continue
		}

		var values []string
		for _, value := range attr.GetValue().GetArrayValue().GetValues() {
			values = append(values, value.GetStringValue())
		}

		return values
	}

	return nil
}

// TestOTelTracesACLIAgentsInvocationAndBridgedTools: a CLI agent makes no chat spans steps can see, so its invoke_agent carries the tokens the CLI reported, and a tool it calls across the bridge is an execute_tool span under it like a hosted agent's.
func TestOTelTracesACLIAgentsInvocationAndBridgedTools(t *testing.T) {
	requireCurl(t)

	collector := newOTLPCollector(t)

	writeFakeClaude(t,
		"echo '"+cliInitEvent(cliPipelineGrantedTools()...)+"'\n"+
			callBridgeScript("verdict", `{"choice":"approve","note":"ship it"}`)+
			"echo '"+cliToolUseEvent("t1", "mcp__steps__verdict", `{"choice":"approve"}`)+"'\n"+
			"echo '"+cliResultEvent("approved", 1)+"'")

	err := cli.Run([]string{cliPipeline(t, t.TempDir())})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	spans, _ := collector.snapshot()
	run := findSpan(t, spans, tracepb.Span_SPAN_KIND_SERVER, "RUN review")
	invoked := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "invoke_agent reviewer")
	tool := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "execute_tool verdict")

	assertUnder(t, run, invoked, tool)
	assertGenAIIdentity(t, tool, "reviewer", spanAttr(run, "cicd.pipeline.run.id"))

	if got := spanAttr(invoked, "gen_ai.provider.name"); got != "anthropic" {
		t.Errorf("gen_ai.provider.name = %q, want anthropic for a claude CLI source", got)
	}

	if in, out := intAttr(invoked, "gen_ai.usage.input_tokens"), intAttr(invoked, "gen_ai.usage.output_tokens"); in != 100 || out != 20 {
		t.Errorf("the CLI invocation reports %d/%d tokens, want the 100/20 the CLI reported", in, out)
	}
}

// TestOTelTracesAFixAgentAsAnInvocation: a fix agent is an agent too, so its model calls sit under an invoke_agent naming it rather than reading as an unknown agent.
func TestOTelTracesAFixAgentAsAnInvocation(t *testing.T) {
	collector := newOTLPCollector(t)

	fake := fixAgentServer(t)
	_ = cli.Run([]string{writeFixPipeline(t, t.TempDir(), fake.URL, "false")})

	spans, _ := collector.snapshot()
	invoked := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "invoke_agent fixer")

	chats := chatsUnder(spans, invoked)
	if len(chats) == 0 {
		t.Fatal("the fix agent's model calls are not under its invoke_agent")
	}

	assertGenAIIdentity(t, chats[0], "fixer", spanAttr(findSpan(t, spans, tracepb.Span_SPAN_KIND_SERVER, "RUN build"), "cicd.pipeline.run.id"))
}

// TestOTelCarriesTheTraceIntoAnMCPCall: the trace context rides the request's params._meta, as the MCP conventions ask, so a server on stdio — which has no HTTP headers to carry it — can still parent its work under the tool call that made it.
func TestOTelCarriesTheTraceIntoAnMCPCall(t *testing.T) {
	collector := newOTLPCollector(t)

	seen := make(chan any, 1)
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "meta-fixture", Version: "v0"}, nil)
	srv.AddTool(&sdkmcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}},
		func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			seen <- req.Params.Meta["traceparent"]

			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "echoed"}}}, nil
		})

	server := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(server.Close)

	fake := newFakeLLM(t, callsTool("hb__echo", map[string]any{}), says("done"))
	mustRun(t, writePipeline(t, t.TempDir(), `
defaults:
  preflight:
    disabled: true

mcp_servers:
- name: hb
  endpoint: `+server.URL+`

agents:
- name: oncall
  source: { model: test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - mcp: hb

jobs:
- name: triage
  plan:
  - agent: oncall
    messages:
      - Echo.
`))

	spans, _ := collector.snapshot()
	tool := findSpan(t, spans, tracepb.Span_SPAN_KIND_INTERNAL, "execute_tool hb__echo")

	want := fmt.Sprintf("00-%x-%x-01", tool.GetTraceId(), tool.GetSpanId())
	if got := <-seen; got != want {
		t.Errorf("params._meta.traceparent = %v, want the tool call's span %s", got, want)
	}
}
