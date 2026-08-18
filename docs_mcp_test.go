package main

// The MCP twin of docs_scenarios_test.go: an executed docs/mcp.md example
// carries mcp=<id> on its fence, naming a fixture here. writeDocBlock starts
// that fixture and rewrites the block's mcp_servers: to its endpoint
// (injectFakeMCP), so the rendered YAML keeps the vendor endpoints a reader
// should see while the test exercises the real check/in/out contract —
// preflight probing included — against an in-process server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// docMCPServer is one started fixture: the URL the pipeline is rewritten to,
// plus every tool call's decoded arguments — recorded for the Go-side checks
// YAML asserts cannot express (what an out: tool actually received).
type docMCPServer struct {
	URL string

	mu    sync.Mutex
	calls map[string][]map[string]any
}

func (s *docMCPServer) record(tool string, args map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls[tool] = append(s.calls[tool], args)
}

// lastCall returns the most recent arguments tool was called with, failing
// the test if it never was.
func (s *docMCPServer) lastCall(t *testing.T, tool string) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()

	calls := s.calls[tool]
	if len(calls) == 0 {
		t.Fatalf("the run never called tool %q", tool)
	}

	return calls[len(calls)-1]
}

// activeDocMCPServer is the fixture writeDocBlock most recently started, how
// runDocBlock's post-run check reaches the same instance the run recorded
// calls on. A package variable is safe because the docs suites are serial
// (t.Setenv forbids t.Parallel), and it keeps writeDocBlock's signature
// stable for the two mutation harnesses that share it.
var activeDocMCPServer *docMCPServer

// docMCPTool is one tool a fixture exposes: the argument names its schema
// declares required — what preflight verifies a stage will send — and the
// text content it answers with, built from the decoded arguments.
type docMCPTool struct {
	name     string
	required []string
	reply    func(args map[string]any) string
}

// startDocMCPServer starts an in-process streamable-HTTP MCP server exposing
// tools, each recording its calls on the returned handle.
func startDocMCPServer(t *testing.T, tools ...docMCPTool) *docMCPServer {
	t.Helper()

	handle := &docMCPServer{calls: map[string][]map[string]any{}}
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "docs-fixture", Version: "v0"}, nil)

	for _, tool := range tools {
		schema := map[string]any{"type": "object"}

		if len(tool.required) > 0 {
			properties := map[string]any{}
			for _, name := range tool.required {
				properties[name] = map[string]any{"type": "string"}
			}

			schema["properties"] = properties
			schema["required"] = tool.required
		}

		srv.AddTool(&sdkmcp.Tool{Name: tool.name, InputSchema: schema},
			func(_ context.Context, req *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
				var args map[string]any

				_ = json.Unmarshal(req.Params.Arguments, &args)
				handle.record(tool.name, args)

				return &sdkmcp.CallToolResult{
					Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: tool.reply(args)}},
				}, nil
			})
	}

	ts := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)

	handle.URL = ts.URL

	return handle
}

// expectDocMCPCall asserts the last call to tool carried exactly want.
// Exact, not subset: an extra argument reaching a remote tool is as much a
// contract break as a missing one.
func expectDocMCPCall(t *testing.T, srv *docMCPServer, tool string, want map[string]any) {
	t.Helper()

	got := srv.lastCall(t, tool)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tool %q was called with %v, want %v", tool, got, want)
	}
}

// docMCPFixture is one fence id's scaffolding: the server its block runs
// against, and (optionally) Go-side checks over what that server received.
type docMCPFixture struct {
	start func(t *testing.T) *docMCPServer
	check func(t *testing.T, srv *docMCPServer)
}

// docMCPFixtures maps fence mcp= ids to their scaffolding. Ids are
// page-scoped, same convention as docScenarios.
var docMCPFixtures = map[string]docMCPFixture{
	// "Backing a resource type with MCP": check answers as a text content
	// block holding an oldest-first JSON array — the RPC mirror of the shell
	// path's "stdout is a JSON array" — and the get takes the newest entry,
	// which the block's assert.stdout pins via version.json.
	"mcp-linear-issues": {
		start: func(t *testing.T) *docMCPServer {
			t.Helper()

			return startDocMCPServer(t,
				docMCPTool{name: "list_issues", required: []string{"team"}, reply: func(map[string]any) string {
					return `[{"id":"ENG-101"},{"id":"ENG-204"}]`
				}},
				docMCPTool{name: "create_issue", required: []string{"title"}, reply: func(map[string]any) string {
					return `{"id":"ENG-501"}`
				}},
			)
		},
		check: func(t *testing.T, srv *docMCPServer) {
			t.Helper()

			// With check.args: unset, the resource's source: IS the argument
			// object — sent verbatim, wrapped in nothing.
			expectDocMCPCall(t, srv, "list_issues", map[string]any{"team": "ENG", "label": "bug"})
		},
	},

	// "Naming the arguments: args:": in.args: lifts the version's fields into
	// the tool's own parameter names, and out.args: does the same for the
	// put's params: — including the text -> message rename.
	"mcp-slack-thread": {
		start: func(t *testing.T) *docMCPServer {
			t.Helper()

			return startDocMCPServer(t,
				docMCPTool{name: "slack_search_public_and_private", required: []string{"query"}, reply: func(map[string]any) string {
					return `[{"channel":"C0123456789","ts":"1717171717.000100"}]`
				}},
				docMCPTool{name: "slack_read_thread", required: []string{"channel_id", "message_ts"}, reply: func(args map[string]any) string {
					return fmt.Sprintf("2 messages in %v at %v", args["channel_id"], args["message_ts"])
				}},
				docMCPTool{name: "slack_send_message", required: []string{"channel_id", "message"}, reply: func(map[string]any) string {
					return `{"ts":"1717171717.000200"}`
				}},
			)
		},
		check: func(t *testing.T, srv *docMCPServer) {
			t.Helper()

			expectDocMCPCall(t, srv, "slack_read_thread", map[string]any{
				"channel_id": "C0123456789", "message_ts": "1717171717.000100",
			})
			expectDocMCPCall(t, srv, "slack_send_message", map[string]any{
				"channel_id": "C0123456789", "thread_ts": "1717171717.000100", "message": "on it",
			})
		},
	},

	// "Sending a file's contents: {file: ...}": the marker resolves to the
	// file's trimmed contents — checked with DeepEqual, so a literal
	// {file: ...} map or an untrimmed trailing newline both fail.
	"mcp-file-params": {
		start: func(t *testing.T) *docMCPServer {
			t.Helper()

			return startDocMCPServer(t,
				docMCPTool{name: "list_issues", required: []string{"team"}, reply: func(map[string]any) string {
					return `[]`
				}},
				docMCPTool{name: "create_issue", required: []string{"title", "description"}, reply: func(map[string]any) string {
					return `{"id":"ENG-501"}`
				}},
			)
		},
		check: func(t *testing.T, srv *docMCPServer) {
			t.Helper()

			expectDocMCPCall(t, srv, "create_issue", map[string]any{
				"title": "Retry loop spins", "description": "the retry loop never backs off",
			})
		},
	},
}
