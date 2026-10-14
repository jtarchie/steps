package e2e

// Shared tools (#219): limits on a tool's parameters, and rules that check a
// call. Each test is one trip through cli.Run with a real MCP client at one end
// and a fake upstream at the other, like e2e_exports_test.go.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
)

// servedPipeline is a daemon holding a pipeline named ops, signed in as alex.
type servedPipeline struct {
	addr string
	dir  string
}

// serveOps sets pipeline (a whole YAML file, with a trivial job appended) on a fresh daemon and returns it. Not for t.Parallel(), for startWeb's reason.
func serveOps(t *testing.T, pipeline string) servedPipeline {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, "defaults:\n  preflight:\n    disabled: true\n"+pipeline+"\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	t.Cleanup(func() { served.stopIfRunning(t) })

	err := cli.Run([]string{"pipeline", "set", "-p", "ops", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	return servedPipeline{addr: served.addr, dir: dir}
}

// connect is an agent's session on one of the pipeline's exports, as alex.
func (s servedPipeline) connect(t *testing.T, export string) *sdkmcp.ClientSession {
	t.Helper()

	token := connectAgent(t, s.addr, export)

	return mcpSession(t, "http://"+s.addr+"/mcp/x/ops/"+export, token)
}

func callTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) *sdkmcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}

	return result
}

func marshalled(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

func publishedSchema(t *testing.T, session *sdkmcp.ClientSession, tool string) map[string]any {
	t.Helper()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, offered := range tools.Tools {
		if offered.Name != tool {
			continue
		}

		raw, err := json.Marshal(offered.InputSchema)
		if err != nil {
			t.Fatal(err)
		}

		var schema map[string]any

		_ = json.Unmarshal(raw, &schema)

		return schema
	}

	t.Fatalf("tool %q is not offered", tool)

	return nil
}

// The limits reach the caller in the schema and bind it whether or not it read them; a refusal never reaches the upstream.
func TestParameterLimitsArePublishedAndEnforcedBeforeTheUpstream(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:       "search_issues",
		required:   []string{"query"},
		properties: map[string]string{"limit": "integer"},
		reply:      func(args map[string]any) string { return "searched " + args["query"].(string) },
	})

	served := serveOps(t, `
mcp_servers:
- name: github
  endpoint: `+upstream.URL+`
mcp_exports:
- name: support
  access: "true"
  tools:
  - mcp: github
    tool: search_issues
    params:
      query: { max_length: 20, pattern: '^[^;]*$' }
      limit: { max: 50, default: 20 }
`)

	session := served.connect(t, "support")

	properties := publishedSchema(t, session, "github__search_issues")["properties"].(map[string]any)
	query, limit := marshalled(t, properties["query"]), marshalled(t, properties["limit"])

	if !strings.Contains(query, `"maxLength":20`) || !strings.Contains(query, `"pattern"`) || !strings.Contains(limit, `"maximum":50`) || !strings.Contains(limit, `"default":20`) {
		t.Errorf("the caller was not shown the limits:\nquery %s\nlimit %s", query, limit)
	}

	for _, refused := range []map[string]any{
		{"query": strings.Repeat("q", 21)},
		{"query": "a;b"},
		{"query": "ok", "limit": 51},
		{"query": "ok", "limit": "many"},
	} {
		result := callTool(t, session, "github__search_issues", refused)
		if !result.IsError {
			t.Errorf("%v was not refused: %s", refused, textOf(result))
		}
	}

	upstream.mu.Lock()
	reached := len(upstream.calls["search_issues"])
	upstream.mu.Unlock()

	if reached != 0 {
		t.Fatalf("%d refused calls reached the upstream", reached)
	}

	result := callTool(t, session, "github__search_issues", map[string]any{"query": "crash"})
	if result.IsError {
		t.Fatalf("a call inside the limits was refused: %s", textOf(result))
	}

	expectDocMCPCall(t, upstream, "search_issues", map[string]any{"query": "crash", "limit": float64(20)})
}
