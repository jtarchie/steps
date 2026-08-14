// Package mcp is the shared MCP (Model Context Protocol) client layer: it
// connects to a configured mcp_servers: entry (config.MCPServer) over either
// Streamable HTTP (endpoint:) or a local subprocess speaking newline-
// delimited JSON on stdin/stdout (command:, see stdio.go), lists its tools,
// and calls one. internal/agent (a tool source for agent steps) and
// internal/resource (a check/in/out backend) both build on this package
// rather than importing the vendored SDK directly, so both share one
// transport/auth implementation.
//
// Bearer and oauth auth are both supported (see config.MCPServerAuth); oauth
// token persistence and the interactive `steps mcp login` bootstrap live in
// oauth.go/login.go respectively.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// clientImplementation identifies steps itself to a connected MCP server.
//
//nolint:gochecknoglobals // static, read-only
var clientImplementation = &sdkmcp.Implementation{Name: "steps", Version: "dev"}

// Client is a live connection to one MCP server: list its tools, call one,
// and close when done. It wraps a *sdkmcp.ClientSession so callers never
// import the vendored SDK directly.
type Client interface {
	ListTools(ctx context.Context) ([]*sdkmcp.Tool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (*sdkmcp.CallToolResult, error)
	Close() error
}

type sessionClient struct {
	session *sdkmcp.ClientSession
}

// maxToolPages bounds how many times ListTools will follow a NextCursor.
//
// The loop's exit condition is supplied by the server, which is the one party
// here that is not trusted to be correct: a cursor that never empties, or one
// that repeats, means the loop never ends and the slice never stops growing.
// A deadline is not the answer, because response caching can serve a repeated
// cursor without a network round trip at all — no request, nothing for a
// cancelled context to interrupt, just an allocation loop until the process
// dies.
//
// A thousand pages is far past any real server (page sizes are typically tens
// to hundreds of tools) and reached in seconds by a broken one, so it
// separates the two cases without having to guess at a legitimate ceiling.
const maxToolPages = 1000

// ListTools returns every tool the server exposes, following pagination
// until the server stops returning a NextCursor.
func (c *sessionClient) ListTools(ctx context.Context) ([]*sdkmcp.Tool, error) {
	var (
		tools  []*sdkmcp.Tool
		cursor string
	)

	// Cursors already followed. A server that cycles A→B→A is not making
	// progress, and noticing that is cheaper and clearer than waiting for the
	// page limit to notice for us.
	seen := make(map[string]struct{})

	for page := range maxToolPages {
		result, err := c.session.ListTools(ctx, &sdkmcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}

		tools = append(tools, result.Tools...)

		if result.NextCursor == "" {
			return tools, nil
		}

		if _, repeated := seen[result.NextCursor]; repeated {
			return nil, fmt.Errorf(
				"mcp: list tools: server repeated pagination cursor %q after %d pages — it is not advancing",
				result.NextCursor, page+1)
		}

		seen[result.NextCursor] = struct{}{}
		cursor = result.NextCursor
	}

	return nil, fmt.Errorf("mcp: list tools: server did not finish paginating after %d pages", maxToolPages)
}

func (c *sessionClient) CallTool(ctx context.Context, name string, args map[string]any) (*sdkmcp.CallToolResult, error) {
	result, err := c.session.CallTool(ctx, &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}

	return result, nil
}

func (c *sessionClient) Close() error {
	err := c.session.Close()
	if err != nil {
		return fmt.Errorf("mcp: close: %w", err)
	}

	return nil
}

// Connect opens a session to srv, over whichever transport it's configured
// for (see config.MCPServer.IsStdio/newTransport). For an HTTP server, per
// its configured auth (see config.MCPServerAuth): "bearer" reads a static
// token from the named env var (mirroring internal/agent's lookupAPIKey
// exactly); "oauth" loads a persisted, self-refreshing token from the
// per-user token store (see oauth.go) — never an interactive flow here,
// that's `steps mcp login` (login.go) alone. "" / "none" connects
// unauthenticated. A stdio server is always unauthenticated by construction
// (config.validateMCPServerTransport rejects any other auth.type at load
// time), since there is no HTTP request to attach a token to.
func Connect(ctx context.Context, srv config.MCPServer) (Client, error) {
	transport, err := newTransport(ctx, srv)
	if err != nil {
		return nil, err
	}

	client := sdkmcp.NewClient(clientImplementation, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connect to %q: %w", srv.Name, err)
	}

	return &sessionClient{session: session}, nil
}

// newTransport selects the transport srv is configured for.
// config.validateMCPServerTransport guarantees exactly one of
// Command/Endpoint is set, so the stdio branch (stdio.go) is the whole
// stdio case; the fallthrough is the original Streamable HTTP path,
// unchanged.
func newTransport(ctx context.Context, srv config.MCPServer) (sdkmcp.Transport, error) {
	if srv.IsStdio() {
		return commandTransport(ctx, srv), nil
	}

	httpClient, err := authorizedHTTPClient(ctx, srv)
	if err != nil {
		return nil, err
	}

	return &sdkmcp.StreamableClientTransport{Endpoint: srv.Endpoint, HTTPClient: httpClient}, nil
}

// authorizedHTTPClient builds the *http.Client Connect's transport uses,
// per srv.Auth.Type.
func authorizedHTTPClient(ctx context.Context, srv config.MCPServer) (*http.Client, error) {
	switch srv.Auth.Type {
	case "", "none":
		return http.DefaultClient, nil
	case "bearer":
		token, err := lookupBearerToken(srv.Auth.APIKeyEnv)
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
		}

		return oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})), nil
	case "oauth":
		ts, err := oauthTokenSource(ctx, srv)
		if err != nil {
			return nil, err
		}

		return oauth2.NewClient(ctx, ts), nil
	default:
		return nil, fmt.Errorf("mcp server %q: unknown auth.type %q", srv.Name, srv.Auth.Type)
	}
}

// lookupBearerToken reads a bearer token from the OS environment variable
// named by envVar — mirrors internal/agent/provider.go's lookupAPIKey
// exactly (a missing/empty variable is a hard error, not a silently blank
// Authorization header).
func lookupBearerToken(envVar string) (string, error) {
	if envVar == "" {
		return "", errors.New("auth.type: bearer requires api_key_env")
	}

	val, ok := os.LookupEnv(envVar)
	if !ok || val == "" {
		return "", fmt.Errorf("environment variable %q (api_key_env) is not set", envVar)
	}

	return val, nil
}
