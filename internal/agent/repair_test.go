package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRepairJSONArgs tables the failures local models actually produce (see
// repair.go's header): each input is what a tool call's raw `arguments`
// string contained; wantOK marks inputs that must come back parseable.
func TestRepairJSONArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string // expected repaired output when wantChanged; "" means "just check it parses"
		changed bool
	}{
		{name: "already valid is untouched", in: `{"path": "a.go"}`, changed: false},
		{name: "empty is untouched", in: ``, changed: false},
		{name: "prose with no object is untouched", in: `I cannot answer that.`, changed: false},
		{
			name:    "truncated mid-string",
			in:      `{"path": "repo/foo.go", "content": "package ma`,
			want:    `{"path": "repo/foo.go", "content": "package ma"}`,
			changed: true,
		},
		{
			name:    "truncated mid-object after colon gets a null",
			in:      `{"path": "a", "line":`,
			want:    `{"path": "a", "line": null}`,
			changed: true,
		},
		{
			name:    "truncated with dangling comma",
			in:      `{"a": 1,`,
			want:    `{"a": 1}`,
			changed: true,
		},
		{
			name:    "trailing comma in a complete object",
			in:      `{"a": 1,}`,
			want:    `{"a": 1}`,
			changed: true,
		},
		{
			name:    "trailing comma in a nested list",
			in:      `{"a": [1, 2,], "b": 2}`,
			want:    `{"a": [1, 2], "b": 2}`,
			changed: true,
		},
		{
			name:    "prose after the object is dropped",
			in:      `{"a": 1} Here is the edit you wanted.`,
			want:    `{"a": 1}`,
			changed: true,
		},
		{
			name:    "markdown fence and preamble are dropped",
			in:      "```json\n{\"a\": 1}\n```",
			want:    `{"a": 1}`,
			changed: true,
		},
		{
			name:    "nested truncation closes every opener",
			in:      `{"a": {"b": [1, 2`,
			want:    `{"a": {"b": [1, 2]}}`,
			changed: true,
		},
		{
			name:    "truncated mid-number",
			in:      `{"n": 12`,
			want:    `{"n": 12}`,
			changed: true,
		},
		{
			name:    "comma inside a string is not treated as trailing",
			in:      `{"a": "x,}"}`,
			changed: false,
		},
		{
			name:    "unrepairable garbage comes back unchanged",
			in:      `{'a': 1}`,
			changed: false,
		},
		{
			name:    "a string value containing braces keeps depth honest",
			in:      `{"a": "}{", "b": 2}`,
			changed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, changed := repairJSONArgs(tt.in)

			if changed != tt.changed {
				t.Errorf("changed = %v, want %v (output %q)", changed, tt.changed, got)
			}

			if !tt.changed {
				if got != tt.in {
					t.Errorf("unchanged input must come back verbatim.\n in: %q\ngot: %q", tt.in, got)
				}

				return
			}

			if tt.want != "" && got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}

			if !validJSONObject(got) {
				t.Errorf("repaired output does not parse: %q", got)
			}
		})
	}
}

// TestRepairChatCompletionBodyValid pins the pass-through discipline: a
// body whose tool calls all parse comes back byte-identically.
func TestRepairChatCompletionBodyValid(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\": \"a.go\"}"}}]}}]}`)

	if got := repairChatCompletionBody(body); !bytes.Equal(got, body) {
		t.Errorf("body changed.\n in: %s\ngot: %s", body, got)
	}
}

// TestRepairChatCompletionBodyRepairs covers the response surgery itself: a
// truncated arguments string comes back repaired with every other field
// intact.
func TestRepairChatCompletionBodyRepairs(t *testing.T) {
	t.Parallel()

	body := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"write_file","arguments":"{\"path\": \"a.go\", \"content\": \"package ma"}}]}}]}`)

	name, args := firstToolCallFunction(t, repairChatCompletionBody(body))

	if name != "write_file" {
		t.Errorf("tool name lost in repair: %q", name)
	}

	var parsed map[string]any

	err := json.Unmarshal([]byte(args), &parsed)
	if err != nil {
		t.Fatalf("arguments still unparseable after repair: %q", args)
	}

	if parsed["path"] != "a.go" || parsed["content"] != "package ma" {
		t.Errorf("repaired args = %v, want path and content preserved", parsed)
	}
}

// firstToolCallFunction extracts the first choice's first tool call's
// function name and raw arguments from a chat-completion body.
func firstToolCallFunction(t *testing.T, body []byte) (name, args string) {
	t.Helper()

	var doc map[string]json.RawMessage

	err := json.Unmarshal(body, &doc)
	if err != nil {
		t.Fatalf("body does not parse: %v\n%s", err, body)
	}

	var choices []map[string]json.RawMessage

	err = json.Unmarshal(doc["choices"], &choices)
	if err != nil {
		t.Fatal(err)
	}

	var message map[string]json.RawMessage

	err = json.Unmarshal(choices[0]["message"], &message)
	if err != nil {
		t.Fatal(err)
	}

	var calls []map[string]json.RawMessage

	err = json.Unmarshal(message["tool_calls"], &calls)
	if err != nil {
		t.Fatal(err)
	}

	var function map[string]json.RawMessage

	err = json.Unmarshal(calls[0]["function"], &function)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(function["name"], &name)
	if err != nil {
		t.Fatal(err)
	}

	err = json.Unmarshal(function["arguments"], &args)
	if err != nil {
		t.Fatal(err)
	}

	return name, args
}

// TestRepairChatCompletionBodyNonObject passes a non-JSON body through.
func TestRepairChatCompletionBodyNonObject(t *testing.T) {
	t.Parallel()

	body := []byte(`not json at all`)
	if got := repairChatCompletionBody(body); !bytes.Equal(got, body) {
		t.Errorf("got %s, want the input untouched", got)
	}
}

// TestRepairTransportRoundTrip proves the wiring: an OpenAI-compat response
// with malformed tool-call arguments is repaired by the transport before the
// client ever parses it.
func TestRepairTransportRoundTrip(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Truncated arguments: the pre-repair adapter would have delivered {}.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"read_file","arguments":"{\"path\": \"a.go\", \"start_line\":"}}]}}]}`))
	}))
	defer server.Close()

	body := doRepairRequest(t, server.URL+"/v1/chat/completions")

	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}

	err := json.Unmarshal(body, &doc)
	if err != nil {
		t.Fatalf("response no longer parses: %v\n%s", err, body)
	}

	args := doc.Choices[0].Message.ToolCalls[0].Function.Arguments

	var parsed map[string]any

	err = json.Unmarshal([]byte(args), &parsed)
	if err != nil {
		t.Fatalf("arguments not repaired by the transport: %q", args)
	}

	if parsed["path"] != "a.go" {
		t.Errorf("args = %v, want the salvaged path", parsed)
	}
}

// TestRepairTransportPassesThroughNonChat proves a non-chat path is left
// byte-identical.
func TestRepairTransportPassesThroughNonChat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer server.Close()

	body := doRepairRequest(t, server.URL+"/v1/models")

	if string(body) != `{"data":[{"id":"m"}]}` {
		t.Errorf("non-chat body changed: %s", body)
	}
}

// doRepairRequest GETs/POSTs url through a client holding only the repair
// transport and returns the response body.
func doRepairRequest(t *testing.T, url string) []byte {
	t.Helper()

	client := &http.Client{Transport: &repairTransport{}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	return body
}
