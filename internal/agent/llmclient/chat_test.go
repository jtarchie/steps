package llmclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

type parityRequest struct {
	name  string
	model string
	req   *model.LLMRequest
}

func ptr[T any](v T) *T { return &v }

func parityRequests() []parityRequest {
	longID := strings.Repeat("x", 60)
	tools := []*genai.Tool{{FunctionDeclarations: []*genai.FunctionDeclaration{
		{Name: "read_file", Description: "reads", ParametersJsonSchema: map[string]any{"type": "OBJECT", "properties": map[string]any{"path": map[string]any{"type": "STRING"}, "opts": map[string]any{"type": "object"}}}},
		{Name: "noargs"},
		{Name: "schema", Parameters: &genai.Schema{Type: genai.TypeObject, Properties: map[string]*genai.Schema{"n": {Type: genai.TypeInteger}}}},
	}}}
	history := []*genai.Content{
		{Role: "user", Parts: []*genai.Part{{Text: "hello"}, {Text: "second"}}},
		{Role: "model", Parts: []*genai.Part{{Text: "thinking hard", Thought: true}, {Text: "answer"}, {FunctionCall: &genai.FunctionCall{ID: "call_1", Name: "read_file", Args: map[string]any{"path": "a.txt"}}}, {FunctionCall: &genai.FunctionCall{ID: longID, Name: "noargs"}}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "call_1", Name: "read_file", Response: map[string]any{"content": "x"}}}, {FunctionResponse: &genai.FunctionResponse{ID: longID, Name: "noargs", Response: map[string]any{}}}}},
		{Role: "model", Parts: []*genai.Part{{Text: "only thought", Thought: true}}},
		{Role: "model", Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "call_2", Name: "read_file", Args: map[string]any{}}}}},
		{Role: "user", Parts: []*genai.Part{{Text: "other agent thought", Thought: true}, {Text: "for context"}}},
		{Role: "user", Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "c3", Name: "adk_request_confirmation", Response: map[string]any{}}}}},
	}
	media := []*genai.Content{{Role: "user", Parts: []*genai.Part{
		{Text: "look"},
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("png")}},
		{InlineData: &genai.Blob{MIMEType: "audio/mpeg", Data: []byte("mp3")}},
		{InlineData: &genai.Blob{MIMEType: "application/pdf", Data: []byte("pdf")}},
		{InlineData: &genai.Blob{MIMEType: "text/plain; charset=utf-8", Data: []byte("txt")}},
		{FileData: &genai.FileData{MIMEType: "image/jpeg", FileURI: "https://x/y.jpg"}},
	}}}
	sys := &genai.Content{Parts: []*genai.Part{{Text: "be brief"}, {Text: "line two"}}}

	out := make([]parityRequest, 0, 14)
	for _, m := range []string{"gpt-4o", "deepseek-chat"} {
		out = append(out,
			parityRequest{"minimal", m, &model.LLMRequest{Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "hi"}}}}, Config: &genai.GenerateContentConfig{MaxOutputTokens: 1}}},
			parityRequest{"history", m, &model.LLMRequest{Contents: history, Config: &genai.GenerateContentConfig{SystemInstruction: sys, Tools: tools}}},
			parityRequest{"media", m, &model.LLMRequest{Contents: media}},
			parityRequest{"dials", m, &model.LLMRequest{Contents: history[:1], Config: &genai.GenerateContentConfig{
				Temperature: ptr[float32](0.25), TopP: ptr[float32](0.5), MaxOutputTokens: 99, StopSequences: []string{"END"},
				ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh}, ResponseMIMEType: "application/json",
				ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAuto}},
			}}},
			parityRequest{"schema", m, &model.LLMRequest{Contents: history[:1], Config: &genai.GenerateContentConfig{
				StopSequences: []string{"a", "b"}, ThinkingConfig: &genai.ThinkingConfig{},
				ResponseSchema: &genai.Schema{Type: genai.TypeObject, Description: "out", Required: []string{"x"}, Properties: map[string]*genai.Schema{"x": {Type: genai.TypeArray, Items: &genai.Schema{Type: genai.TypeString, Enum: []string{"p", "q"}}}}},
				Tools:          tools,
				ToolConfig:     &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny, AllowedFunctionNames: []string{"read_file"}}},
			}}},
			parityRequest{"none", m, &model.LLMRequest{Contents: history[:1], Config: &genai.GenerateContentConfig{Tools: tools, ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelLow},
				ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeNone}}}}},
			parityRequest{"required", m, &model.LLMRequest{Contents: history[:1], Config: &genai.GenerateContentConfig{Tools: tools,
				ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{Mode: genai.FunctionCallingConfigModeAny}}}}},
		)
	}

	return out
}

var parityResponses = map[string]struct {
	status int
	body   string
}{
	"full":       {200, `{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"hello","reasoning_content":"rc","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}},{"id":"t2","type":"function","function":{"name":"noargs","arguments":""}},{"id":"t3","type":"function","function":{"name":"bad","arguments":"{not json"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`},
	"reasoning":  {200, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":null,"reasoning":"r only"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`},
	"nousage":    {200, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"x"}}]}`},
	"filter":     {200, `{"choices":[{"finish_reason":"content_filter","message":{"content":""}}],"usage":{"total_tokens":0}}`},
	"other":      {200, `{"choices":[{"finish_reason":"weird","message":{"content":"y"}}]}`},
	"nullreason": {200, `{"choices":[{"finish_reason":null,"message":{"content":"z","tool_calls":null}}]}`},
	"nochoices":  {200, `{"choices":[]}`},
	"err500":     {500, `{"error":{"message":"boom","type":"server_error","code":"x"}}`},
	"err400":     {400, `{"error":{"message":"bad","type":"invalid_request_error","param":"messages","code":"y"}}`},
	"err429":     {429, `not json`},
}

func normalizeJSON(t *testing.T, raw []byte) any {
	t.Helper()

	var v any

	err := json.Unmarshal(raw, &v)
	if err != nil {
		return string(raw)
	}

	return v
}

// TestChatWireMatchesTheOpenAIClient holds chat.go to what openai-go and adk-utils-go's adapter put on the wire and made of the answer, recorded from them before they were replaced (testdata/chat_wire.json): every request body, the auth header, every converted response, and how each error classifies for fallback:. STEPS_UPDATE_GOLDEN=1 rewrites it, which is a decision to diverge from that reading.
func TestChatWireMatchesTheOpenAIClient(t *testing.T) {
	t.Parallel()

	record := map[string]any{}
	recordRequests(t, record)
	recordResponses(t, record)

	golden := filepath.Join("testdata", "chat_wire.json")

	if os.Getenv("STEPS_UPDATE_GOLDEN") != "" {
		err := os.WriteFile(golden, append(mustJSON(t, record), '\n'), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]any{}
	got := map[string]any{}

	raw, err := os.ReadFile(golden) //nolint:gosec // this test's own testdata
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(raw, &want)
	if err == nil {
		err = json.Unmarshal(mustJSON(t, record), &got)
	}

	if err != nil {
		t.Fatal(err)
	}

	for key := range want {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s:\n got  %s\n want %s", key, mustJSON(t, got[key]), mustJSON(t, want[key]))
		}
	}

	if len(got) != len(want) {
		t.Errorf("%d cases, want %d", len(got), len(want))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	encoded, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}

	return encoded
}

// recordRequests sends every parity request and keeps the body and headers each put on the wire.
func recordRequests(t *testing.T, record map[string]any) {
	t.Helper()

	for _, pr := range parityRequests() {
		var (
			body    []byte
			headers http.Header
		)

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ = io.ReadAll(r.Body)
			headers = r.Header.Clone()

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(parityResponses["nousage"].body))
		}))

		llm := New(config.ResolvedInvocation{BaseURL: server.URL + "/v1/", ModelName: pr.model, AgentName: "a", Attempts: 1}, "k")
		for range llm.GenerateContent(context.Background(), pr.req, false) {
		}

		server.Close()

		record["req/"+pr.model+"/"+pr.name] = map[string]any{"body": normalizeJSON(t, body), "auth": headers.Get("Authorization"), "ctype": headers.Get("Content-Type")}
	}
}

// recordResponses answers one request with every canned response and keeps what the client made of each.
func recordResponses(t *testing.T, record map[string]any) {
	t.Helper()

	names := make([]string, 0, len(parityResponses))
	for name := range parityResponses {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, m := range []string{"gpt-4o", "deepseek-chat"} {
		for _, name := range names {
			canned := parityResponses[name]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(canned.status)
				_, _ = w.Write([]byte(canned.body))
			}))

			llm := New(config.ResolvedInvocation{BaseURL: server.URL + "/v1/", ModelName: m, AgentName: "a", Attempts: 1}, "k")

			var got []any

			for resp, err := range llm.GenerateContent(context.Background(), parityRequests()[0].req, false) {
				if err != nil {
					got = append(got, map[string]any{"transient": isTransientProviderError(err), "rejection": isProviderRejection(err)})

					continue
				}

				got = append(got, normalizeJSON(t, mustJSON(t, resp)))
			}

			server.Close()

			record["resp/"+m+"/"+name] = got
		}
	}
}

// TestAKeylessSourceSendsNoCredential: LookupAPIKey's "" means no Authorization header, and openai-go broke that by reading OPENAI_API_KEY from the environment and sending it to whatever local endpoint the source named.
func TestAKeylessSourceSendsNoCredential(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "a-key-for-somewhere-else")

	var auth []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Values("Authorization")
		_, _ = w.Write([]byte(parityResponses["nousage"].body))
	}))
	t.Cleanup(server.Close)

	llm := New(config.ResolvedInvocation{BaseURL: server.URL + "/v1/", ModelName: "local", Attempts: 1}, "")
	for _, err := range llm.GenerateContent(context.Background(), parityRequests()[0].req, false) {
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(auth) != 0 {
		t.Errorf("Authorization = %q, want none", auth)
	}
}
