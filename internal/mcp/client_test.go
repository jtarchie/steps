package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
)

// echoServer returns an *sdkmcp.Server exposing one "echo" tool that
// returns its "text" argument back as both text and structured content.
func echoServer() *sdkmcp.Server {
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "echo", Version: "test"}, nil)

	srv.AddTool(&sdkmcp.Tool{
		Name:        "echo",
		Description: "Echoes its text argument.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
	}, func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}

		_ = json.Unmarshal(req.Params.Arguments, &args)

		return &sdkmcp.CallToolResult{
			Content:           []sdkmcp.Content{&sdkmcp.TextContent{Text: args.Text}},
			StructuredContent: map[string]any{"text": args.Text},
		}, nil
	})

	return srv
}

// requireBearer wraps handler, rejecting any request whose Authorization
// header isn't exactly "Bearer <want>". challenge supplies the 401's
// WWW-Authenticate value, read per request so a test can model a server
// that spells it unusually (see TestLoginSpaceSeparatedChallenge).
func requireBearer(want string, challenge func() string, handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+want {
			w.Header().Set("WWW-Authenticate", challenge())
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		handler.ServeHTTP(w, r)
	})
}

func TestConnectListToolsCallTool(t *testing.T) {
	t.Parallel()

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	client, err := Connect(context.Background(), config.MCPServer{Name: "echo", Endpoint: ts.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, want one tool named echo", tools)
	}

	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hi"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	if result.IsError {
		t.Fatalf("CallTool result is an error: %+v", result)
	}

	sc, ok := result.StructuredContent.(map[string]any)
	if !ok || sc["text"] != "hi" {
		t.Fatalf("StructuredContent = %+v, want {text: hi}", result.StructuredContent)
	}
}

func TestConnectBearerAuth(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	ts := httptest.NewServer(requireBearer("s3cr3t", func() string { return "Bearer" }, handler))
	t.Cleanup(ts.Close)

	t.Setenv("TEST_MCP_TOKEN", "s3cr3t")

	srv := config.MCPServer{
		Name:     "echo",
		Endpoint: ts.URL,
		Auth:     config.MCPServerAuth{Type: "bearer", APIKeyEnv: "TEST_MCP_TOKEN"}, //nolint:gosec // env-var *name*, not a credential value
	}

	client, err := Connect(context.Background(), srv)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = client.Close() }()

	_, err = client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
}

func TestConnectBearerAuthWrongToken(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	ts := httptest.NewServer(requireBearer("s3cr3t", func() string { return "Bearer" }, handler))
	t.Cleanup(ts.Close)

	t.Setenv("TEST_MCP_TOKEN", "wrong")

	srv := config.MCPServer{
		Name:     "echo",
		Endpoint: ts.URL,
		Auth:     config.MCPServerAuth{Type: "bearer", APIKeyEnv: "TEST_MCP_TOKEN"}, //nolint:gosec // env-var *name*, not a credential value
	}

	_, err := Connect(context.Background(), srv)
	if err == nil {
		t.Fatal("Connect: expected an error with the wrong bearer token")
	}
}

func TestConnectBearerAuthMissingEnv(t *testing.T) {
	t.Parallel()

	srv := config.MCPServer{
		Name:     "echo",
		Endpoint: "http://127.0.0.1:0",
		Auth:     config.MCPServerAuth{Type: "bearer", APIKeyEnv: "STEPS_TEST_MCP_TOKEN_UNSET"}, //nolint:gosec // env-var *name*, not a credential value
	}

	_, err := Connect(context.Background(), srv)
	if err == nil {
		t.Fatal("Connect: expected an error when api_key_env is unset")
	}
}

func TestListServerTools(t *testing.T) {
	t.Parallel()

	handler := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return echoServer() }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	tools, err := ListServerTools(context.Background(), config.MCPServer{Name: "echo", Endpoint: ts.URL})
	if err != nil {
		t.Fatalf("ListServerTools: %v", err)
	}

	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListServerTools = %+v", tools)
	}
}

// paginatingServer returns an echo server whose tools/list always answers
// with a NextCursor, modelling a server whose pagination never terminates.
// nextCursor decides what that cursor is, so a test can pick between a cursor
// that always advances and one that repeats.
func paginatingServer(nextCursor func(page int) string) *sdkmcp.Server {
	srv := echoServer()

	page := 0

	srv.AddReceivingMiddleware(func(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
		return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
			// The cursors handed out below are invented, so the real handler
			// would reject them as invalid params before the client's loop is
			// ever the thing under test. Clear it on the way in: this fake's
			// job is the NextCursor it returns, not honouring one.
			if listReq, ok := req.(*sdkmcp.ListToolsRequest); ok && listReq.Params != nil {
				listReq.Params.Cursor = ""
			}

			result, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return result, err
			}

			listed, ok := result.(*sdkmcp.ListToolsResult)
			if !ok {
				return result, err
			}

			page++
			listed.NextCursor = nextCursor(page)

			return listed, nil
		}
	})

	return srv
}

// TestListToolsStopsOnAnEndlessCursor pins the bound on a loop whose exit
// condition is supplied by the server being talked to.
//
// A cursor that never empties meant ListTools never returned and its slice
// never stopped growing. A caller deadline is not a sufficient answer:
// response caching can serve a repeated cursor without a network round trip,
// leaving nothing for a cancelled context to interrupt.
func TestListToolsStopsOnAnEndlessCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		nextCursor func(page int) string
		wantErr    string
	}{{
		name:       "a cursor that always advances",
		nextCursor: func(page int) string { return fmt.Sprintf("page-%d", page) },
		wantErr:    "did not finish paginating",
	}, {
		name:       "a cursor that cycles",
		nextCursor: func(page int) string { return fmt.Sprintf("page-%d", page%2) },
		wantErr:    "not advancing",
	}}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			handler := sdkmcp.NewStreamableHTTPHandler(
				func(*http.Request) *sdkmcp.Server { return paginatingServer(test.nextCursor) }, nil)
			ts := httptest.NewServer(handler)
			t.Cleanup(ts.Close)

			client, err := Connect(context.Background(), config.MCPServer{Name: "endless", Endpoint: ts.URL})
			if err != nil {
				t.Fatalf("Connect: %v", err)
			}
			t.Cleanup(func() { _ = client.Close() })

			_, err = client.ListTools(context.Background())
			if err == nil {
				t.Fatal("ListTools returned no error for a server that never finishes paginating")
			}

			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ListTools error = %v, want it to mention %q", err, test.wantErr)
			}
		})
	}
}
