package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// serveVercelCapturing uses agentHTTPClient's real stack for a Vercel base URL so the test crosses resolve→transport instead of hand-building the transport.
func serveVercelCapturing(t *testing.T) (*http.Client, string, *http.Header, *map[string]any) {
	t.Helper()

	var (
		header http.Header
		body   map[string]any
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}

		header = r.Header.Clone()
		body = nil
		_ = json.Unmarshal(raw, &body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	client := agentHTTPClient(config.ResolvedInvocation{
		BaseURL:   "https://ai-gateway.vercel.sh/v1/",
		ModelName: "anthropic/claude-sonnet-5",
		AgentName: "reviewer",
	})

	return client, server.URL, &header, &body
}

func TestVercelTransportWireMutations(t *testing.T) {
	t.Parallel()

	t.Run("stamps session affinity and automatic caching on a chat completion", func(t *testing.T) {
		t.Parallel()

		client, base, header, body := serveVercelCapturing(t)
		ctx := WithNewRun(t.Context(), "job")

		postJSON(ctx, t, client, base+"/v1/chat/completions",
			`{"model":"openai/gpt-5","messages":[{"role":"user","content":"<transition_context>"}]}`)

		assertSessionShape(t, header.Get("x-session-affinity"), "job", "reviewer")

		options, _ := (*body)["providerOptions"].(map[string]any)
		gateway, _ := options["gateway"].(map[string]any)

		if gateway["caching"] != "auto" {
			t.Errorf("providerOptions = %v, want gateway.caching auto", (*body)["providerOptions"])
		}

		if _, present := (*body)["cache_control"]; present {
			t.Error("the OpenRouter cache_control marker was sent to Vercel")
		}
	})

	t.Run("leaves caller-supplied providerOptions alone", func(t *testing.T) {
		t.Parallel()

		client, base, _, body := serveVercelCapturing(t)

		postJSON(t.Context(), t, client, base+"/v1/chat/completions",
			`{"model":"anthropic/claude-sonnet-5","providerOptions":{"gateway":{"order":["vertex"]}}}`)

		options, _ := (*body)["providerOptions"].(map[string]any)
		gateway, _ := options["gateway"].(map[string]any)

		if _, present := gateway["caching"]; present {
			t.Errorf("providerOptions = %v, want the caller's untouched", options)
		}
	})

	t.Run("sends no affinity header outside a run", func(t *testing.T) {
		t.Parallel()

		client, base, header, _ := serveVercelCapturing(t)

		postJSON(t.Context(), t, client, base+"/v1/chat/completions", `{"model":"openai/gpt-5"}`)

		if got := header.Get("x-session-affinity"); got != "" {
			t.Errorf("x-session-affinity = %q, want no header", got)
		}
	})

	t.Run("leaves a non-chat path alone", func(t *testing.T) {
		t.Parallel()

		client, base, header, body := serveVercelCapturing(t)

		postJSON(WithNewRun(t.Context(), "job"), t, client, base+"/v1/models", `{"model":"openai/gpt-5"}`)

		if header.Get("x-session-affinity") != "" {
			t.Error("x-session-affinity was stamped on a non-chat request")
		}

		if _, present := (*body)["providerOptions"]; present {
			t.Error("providerOptions was stamped on a non-chat request")
		}
	})
}

func TestIsVercelBaseURL(t *testing.T) {
	t.Parallel()

	for baseURL, want := range map[string]bool{
		"https://ai-gateway.vercel.sh/v1/":     true,
		"https://AI-Gateway.Vercel.sh/v1/":     true,
		"https://notai-gateway.vercel.sh/v1/":  false,
		"https://ai-gateway.vercel.sh.evil/v1": false,
		"https://openrouter.ai/api/v1/":        false,
		"":                                     false,
	} {
		if got := isVercelBaseURL(baseURL); got != want {
			t.Errorf("isVercelBaseURL(%q) = %v, want %v", baseURL, got, want)
		}
	}
}

func TestVercelTransportKeepsHTMLUnescaped(t *testing.T) {
	t.Parallel()

	got := string(withAutoCaching([]byte(`{"model":"m","messages":"<a>&"}`)))
	if !strings.Contains(got, `"<a>&"`) {
		t.Errorf("withAutoCaching = %s, want spliced-through bytes unescaped", got)
	}
}
