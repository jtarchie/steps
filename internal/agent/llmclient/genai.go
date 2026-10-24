package llmclient

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Invocation is one agent invocation as the OpenTelemetry GenAI conventions see it: the agent every span under it belongs to, the conversation those spans group by, and the model and tool calls it made itself — a sub-agent is an Invocation of its own, so each call is counted once across the tree.
type Invocation struct {
	Agent        string
	Conversation string
	inference    atomic.Int64
	tools        atomic.Int64
}

type invocationKey struct{}

// WithInvocation scopes ctx to inv, so every chat and execute_tool span under it says which agent emitted it.
func WithInvocation(ctx context.Context, inv *Invocation) context.Context {
	return context.WithValue(ctx, invocationKey{}, inv)
}

// InvocationFrom is the invocation ctx runs under, nil outside one (preflight probes, tests).
func InvocationFrom(ctx context.Context) *Invocation {
	inv, _ := ctx.Value(invocationKey{}).(*Invocation)

	return inv
}

// Attributes is the identity every GenAI span under inv carries: Honeycomb's agent timeline shows an "Unknown" agent for a span without gen_ai.agent.name.
func (inv *Invocation) Attributes() []attribute.KeyValue {
	if inv == nil {
		return nil
	}

	attrs := []attribute.KeyValue{attribute.String("gen_ai.agent.name", inv.Agent)}
	if inv.Conversation != "" {
		attrs = append(attrs, attribute.String("gen_ai.conversation.id", inv.Conversation))
	}

	return attrs
}

// CountTool records one tool call the invocation made, rejected ones included, as gen_ai.invoke_agent.tool_calls counts them.
func (inv *Invocation) CountTool() {
	if inv != nil {
		inv.tools.Add(1)
	}
}

func (inv *Invocation) countInference() {
	if inv != nil {
		inv.inference.Add(1)
	}
}

// Counts is how many model and tool calls the invocation made itself.
func (inv *Invocation) Counts() (inference, tools int64) {
	return inv.inference.Load(), inv.tools.Load()
}

// CaptureContent reports whether prompts, responses and tool arguments and results go on spans. Off unless asked for, as the conventions require: they can carry secrets, and nothing redacts them.
func CaptureContent() bool {
	on, _ := strconv.ParseBool(os.Getenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT"))

	return on
}

// ponytail: exact hosts only — Azure and Bedrock endpoints are per-account hostnames and an unknown host reports no provider; match suffixes when someone runs one.
var providerHosts = map[string]string{ //nolint:gochecknoglobals // static, read-only lookup table
	"api.openai.com":                    "openai",
	"api.anthropic.com":                 "anthropic",
	"api.groq.com":                      "groq",
	"api.mistral.ai":                    "mistral_ai",
	"api.deepseek.com":                  "deepseek",
	"api.x.ai":                          "x_ai",
	"api.perplexity.ai":                 "perplexity",
	"api.cohere.com":                    "cohere",
	"generativelanguage.googleapis.com": "gcp.gemini",
	"openrouter.ai":                     "openrouter",
	"api.together.xyz":                  "together",
}

// ProviderName is gen_ai.provider.name for an endpoint, "" when the host is not one steps recognizes.
func ProviderName(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}

	return providerHosts[parsed.Hostname()]
}

// finishReason is a response's finish reason in the conventions' vocabulary. It reads the response rather than convertFinishReason's fold, which reports a tool call as STOP.
func finishReason(resp *model.LLMResponse) string {
	switch resp.FinishReason { //nolint:exhaustive // the rest carry no reason the conventions name
	case genai.FinishReasonStop:
		if hasToolCall(resp.Content) {
			return "tool_call"
		}

		return "stop"
	case genai.FinishReasonMaxTokens:
		return "length"
	case genai.FinishReasonSafety:
		return "content_filter"
	default:
		return ""
	}
}

func hasToolCall(content *genai.Content) bool {
	if content == nil {
		return false
	}

	for _, part := range content.Parts {
		if part.FunctionCall != nil {
			return true
		}
	}

	return false
}

// message is one entry of gen_ai.input.messages/gen_ai.output.messages, in the conventions' JSON schema.
type message struct {
	Role         string           `json:"role"`
	Parts        []map[string]any `json:"parts"`
	FinishReason string           `json:"finish_reason,omitempty"`
}

func inputMessages(contents []*genai.Content) string {
	messages := make([]message, 0, len(contents))

	for _, content := range contents {
		if content == nil {
			continue
		}

		messages = append(messages, message{Role: messageRole(content), Parts: messageParts(content)})
	}

	return marshalContent(messages)
}

func systemInstructions(content *genai.Content) string {
	if content == nil {
		return ""
	}

	return marshalContent(messageParts(content))
}

func outputMessage(parts []map[string]any, reason string) string {
	return marshalContent([]message{{Role: "assistant", Parts: parts, FinishReason: reason}})
}

// messageRole maps genai's two roles onto the conventions' four: a user turn that only answers tool calls is the tool speaking.
func messageRole(content *genai.Content) string {
	if content.Role == genai.RoleModel {
		return "assistant"
	}

	for _, part := range content.Parts {
		if part.FunctionResponse == nil {
			return "user"
		}
	}

	if len(content.Parts) == 0 {
		return "user"
	}

	return "tool"
}

func messageParts(content *genai.Content) []map[string]any {
	parts := make([]map[string]any, 0, len(content.Parts))

	for _, part := range content.Parts {
		switch {
		case part.FunctionCall != nil:
			parts = append(parts, map[string]any{"type": "tool_call", "id": part.FunctionCall.ID, "name": part.FunctionCall.Name, "arguments": part.FunctionCall.Args})
		case part.FunctionResponse != nil:
			parts = append(parts, map[string]any{"type": "tool_call_response", "id": part.FunctionResponse.ID, "response": part.FunctionResponse.Response})
		case part.InlineData != nil:
			modality, _, _ := strings.Cut(part.InlineData.MIMEType, "/")
			parts = append(parts, map[string]any{"type": "blob", "modality": modality, "mime_type": part.InlineData.MIMEType, "content": part.InlineData.Data})
		case part.Thought:
			parts = append(parts, map[string]any{"type": "reasoning", "content": part.Text})
		case part.Text != "":
			parts = append(parts, map[string]any{"type": "text", "content": part.Text})
		}
	}

	return parts
}

// marshalContent is v as the JSON string the conventions fall back to on spans, which cannot yet hold structured values in Go.
func marshalContent(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}

	return string(raw)
}
