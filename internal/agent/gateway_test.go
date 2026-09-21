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

// serveGatewayCapturing uses agentHTTPClient's real stack for baseURL so the test crosses resolve→transport instead of hand-building the transport.
func serveGatewayCapturing(t *testing.T, baseURL string) (*http.Client, string, *http.Header, *map[string]any) {
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
		BaseURL:   baseURL,
		ModelName: "anthropic/claude-sonnet-5",
		AgentName: "reviewer",
	})

	return client, server.URL, &header, &body
}

func TestGatewayTransportWireMutations(t *testing.T) {
	t.Parallel()

	gateways := []gatewayWireCase{
		{
			name:          "vercel",
			baseURL:       "https://ai-gateway.vercel.sh/v1/",
			sessionHeader: "x-session-affinity",
			field:         "providerOptions",
			assertCaching: func(t *testing.T, value any) {
				t.Helper()

				options, _ := value.(map[string]any)
				gateway, _ := options["gateway"].(map[string]any)

				if gateway["caching"] != "auto" {
					t.Errorf("providerOptions = %v, want gateway.caching auto", value)
				}
			},
		},
		{
			// No caching field: opencode's gateway routes to providers that cache implicitly (its own response reports prompt_cache_hit_tokens), so the session header is the whole of what it wants. Without one it answers 400 MissingSessionID and refuses the request outright, which makes this the one gateway here whose header is required rather than an optimization.
			name:          "opencode",
			baseURL:       "https://opencode.ai/zen/go/v1/",
			sessionHeader: "x-opencode-session",
		},
		{
			name:    "requesty",
			baseURL: "https://router.requesty.ai/v1/",
			field:   "requesty",
			assertCaching: func(t *testing.T, value any) {
				t.Helper()

				options, _ := value.(map[string]any)

				if options["auto_cache"] != true {
					t.Errorf("requesty = %v, want auto_cache true", value)
				}
			},
		},
	}

	for _, gw := range gateways {
		assertGatewayWire(t, gw)
	}

	t.Run("vercel sends no affinity header outside a run", func(t *testing.T) {
		t.Parallel()

		client, base, header, _ := serveGatewayCapturing(t, "https://ai-gateway.vercel.sh/v1/")

		postJSON(t.Context(), t, client, base+"/v1/chat/completions", `{"model":"m"}`)

		if got := header.Get("x-session-affinity"); got != "" {
			t.Errorf("x-session-affinity = %q, want no header", got)
		}
	})

	t.Run("helicone gets no caching layer", func(t *testing.T) {
		t.Parallel()

		client, base, _, body := serveGatewayCapturing(t, "https://ai-gateway.helicone.ai/v1/")

		postJSON(WithNewRun(t.Context(), "job"), t, client, base+"/v1/chat/completions", `{"model":"m"}`)

		if len(*body) != 1 {
			t.Errorf("body = %v, want it sent as written", *body)
		}
	})
}

type gatewayWireCase struct {
	name          string
	baseURL       string
	sessionHeader string
	// field is the body key this gateway's automatic caching is spelled in, or "" for a gateway that needs no body change at all — where the assertion becomes that the body arrives exactly as written.
	field         string
	assertCaching func(t *testing.T, value any)
}

func assertGatewayWire(t *testing.T, gw gatewayWireCase) {
	t.Helper()

	assertGatewayStampsAChatCompletion(t, gw)
	assertGatewayKeepsACallerSuppliedField(t, gw)
	assertGatewayLeavesANonChatPathAlone(t, gw)
}

func assertGatewayStampsAChatCompletion(t *testing.T, gw gatewayWireCase) {
	t.Helper()

	t.Run(gw.name+" stamps session and automatic caching on a chat completion", func(t *testing.T) {
		t.Parallel()

		client, base, header, body := serveGatewayCapturing(t, gw.baseURL)

		postJSON(WithNewRun(t.Context(), "job"), t, client, base+"/v1/chat/completions",
			`{"model":"openai/gpt-5","messages":[{"role":"user","content":"<transition_context>"}]}`)

		if gw.sessionHeader != "" {
			assertSessionShape(t, header.Get(gw.sessionHeader), "job", "reviewer")
		}

		assertGatewayCaching(t, gw, *body)

		if _, present := (*body)["cache_control"]; present {
			t.Error("the OpenRouter cache_control marker was sent to a caching gateway")
		}

		if header.Get("x-session-id") != "" {
			t.Error("the OpenRouter session header was sent to a caching gateway")
		}
	})
}

// assertGatewayCaching holds the body to what this gateway's spelling asks for — or, for one with no caching field, to arriving exactly as written.
func assertGatewayCaching(t *testing.T, gw gatewayWireCase, body map[string]any) {
	t.Helper()

	if gw.field != "" {
		gw.assertCaching(t, body[gw.field])

		return
	}

	if len(body) != 2 {
		t.Errorf("body = %v, want the two fields it was sent with and nothing added", body)
	}

	if _, present := body[""]; present {
		t.Error(`the body carries a "" key: an empty caching field was spliced in as one`)
	}
}

func assertGatewayKeepsACallerSuppliedField(t *testing.T, gw gatewayWireCase) {
	t.Helper()

	if gw.field == "" {
		return // no body field, so there is none for a caller to have supplied
	}

	t.Run(gw.name+" leaves a caller-supplied field alone", func(t *testing.T) {
		t.Parallel()

		client, base, _, body := serveGatewayCapturing(t, gw.baseURL)

		postJSON(t.Context(), t, client, base+"/v1/chat/completions",
			`{"model":"m","`+gw.field+`":{"mine":true}}`)

		got, _ := (*body)[gw.field].(map[string]any)
		if len(got) != 1 || got["mine"] != true {
			t.Errorf("%s = %v, want the caller's untouched", gw.field, got)
		}
	})
}

func assertGatewayLeavesANonChatPathAlone(t *testing.T, gw gatewayWireCase) {
	t.Helper()

	t.Run(gw.name+" leaves a non-chat path alone", func(t *testing.T) {
		t.Parallel()

		client, base, header, body := serveGatewayCapturing(t, gw.baseURL)

		postJSON(WithNewRun(t.Context(), "job"), t, client, base+"/v1/models", `{"model":"m"}`)

		if gw.sessionHeader != "" && header.Get(gw.sessionHeader) != "" {
			t.Error("session header was stamped on a non-chat request")
		}

		if gw.field != "" {
			if _, present := (*body)[gw.field]; present {
				t.Errorf("%s was stamped on a non-chat request", gw.field)
			}
		}
	})
}

func TestCachingGatewayFor(t *testing.T) {
	t.Parallel()

	for baseURL, want := range map[string]string{
		"https://opencode.ai/zen/go/v1/":       "opencode",
		"https://notopencode.ai/zen/go/v1/":    "",
		"https://ai-gateway.vercel.sh/v1/":     "vercel",
		"https://AI-Gateway.Vercel.sh/v1/":     "vercel",
		"https://router.requesty.ai/v1/":       "requesty",
		"https://router.eu.requesty.ai/v1/":    "requesty",
		"https://notai-gateway.vercel.sh/v1/":  "",
		"https://ai-gateway.vercel.sh.evil/v1": "",
		"https://notrequesty.ai/v1/":           "",
		"https://ai-gateway.helicone.ai/v1/":   "",
		"https://openrouter.ai/api/v1/":        "",
		"":                                     "",
	} {
		gateway, _ := cachingGatewayFor(baseURL)
		if gateway.name != want {
			t.Errorf("cachingGatewayFor(%q) = %q, want %q", baseURL, gateway.name, want)
		}
	}
}

func TestGatewayCachingKeepsHTMLUnescaped(t *testing.T) {
	t.Parallel()

	got := string(cachingGateways[0].withAutoCaching([]byte(`{"model":"m","messages":"<a>&"}`)))
	if !strings.Contains(got, `"<a>&"`) {
		t.Errorf("withAutoCaching = %s, want spliced-through bytes unescaped", got)
	}
}
