package merkle

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// mcpAgentCfg builds a Config with an `mcp_servers:` entry named endpoint,
// plus a `reviewer` agent granting the tools in reviewerTools.
func mcpAgentCfg(reviewerTools []config.ToolSpec, endpoint string) *config.Config {
	return &config.Config{
		MCPServers: []config.MCPServer{{Name: "github", Endpoint: endpoint}},
		Agents: []config.Agent{
			{Name: "reviewer", Source: config.AgentSource{Model: "lmstudio/qwen"}, Tools: reviewerTools},
		},
	}
}

func TestMCPToolGrantHashChangesVsBuiltin(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	withBuiltin := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{Builtin: "read_file"}}, "https://api.github.com/mcp/"), step)
	withMCP := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTool: "search_issues"}}, "https://api.github.com/mcp/"), step)

	if withBuiltin == withMCP {
		t.Error("granting an mcp tool should change the hash vs a builtin grant")
	}
}

// TestMCPServerEndpointChangeBustsHash proves that editing the referenced
// server's endpoint changes an agent step's hash, even though the tool
// grant itself (mcp: github, tool: search_issues) is textually unchanged —
// the server identity is folded in via mcpServerContent.
func TestMCPServerEndpointChangeBustsHash(t *testing.T) {
	t.Parallel()

	tools := []config.ToolSpec{{MCP: "github", MCPTool: "search_issues"}}
	step := config.Step{Agent: "reviewer", Prompt: "do it"}

	before := mustAgentHash(t, mcpAgentCfg(tools, "https://api.github.com/mcp/v1/"), step)
	after := mustAgentHash(t, mcpAgentCfg(tools, "https://api.github.com/mcp/v2/"), step)

	if before == after {
		t.Error("editing the mcp server's endpoint should bust the hash, but hashes matched")
	}
}

func TestMCPToolNameChangeBustsHash(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}
	endpoint := "https://api.github.com/mcp/"

	before := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTool: "search_issues"}}, endpoint), step)
	after := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTool: "get_issue"}}, endpoint), step)

	if before == after {
		t.Error("changing the granted mcp tool name should bust the hash, but hashes matched")
	}
}

// TestMCPSubsetGrantHashOrderIndependent proves the named-subset form
// (tools: [...]) hashes identically regardless of declaration order — the
// sorted-copy handling in mcpToolSpecContent.
func TestMCPSubsetGrantHashOrderIndependent(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}
	endpoint := "https://api.github.com/mcp/"

	a := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTools: []string{"get_issue", "search_issues"}}}, endpoint), step)
	b := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTools: []string{"search_issues", "get_issue"}}}, endpoint), step)

	if a != b {
		t.Error("a named-subset mcp grant should hash the same regardless of declaration order")
	}
}

// TestMCPGrantFormsHashDifferently proves the three grant forms (single
// tool, named subset, bare "all") don't collide with each other even when
// they reference overlapping tool names.
func TestMCPGrantFormsHashDifferently(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "do it"}
	endpoint := "https://api.github.com/mcp/"

	single := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTool: "search_issues"}}, endpoint), step)
	subset := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github", MCPTools: []string{"search_issues"}}}, endpoint), step)
	all := mustAgentHash(t, mcpAgentCfg([]config.ToolSpec{{MCP: "github"}}, endpoint), step)

	if single == subset || single == all || subset == all {
		t.Errorf("the three mcp grant forms must hash distinctly: single=%s subset=%s all=%s", single, subset, all)
	}
}

// mcpResourceCfg builds a Config with an mcp-backed resource type ("linear-
// issues") whose check/in/out tool names are as given, plus a resource
// instance and a job with a get (and, when withPut, a put) step.
func mcpResourceCfg(checkTool, inTool string, withPut bool) (*config.Config, config.Step) {
	rt := config.ResourceType{
		Name: "linear-issues",
		Config: config.ResourceTypeConfig{
			MCP: &config.MCPResourceConfig{
				Server: "linear",
				Check:  &config.MCPToolCall{Tool: checkTool},
			},
		},
	}

	if inTool != "" {
		rt.Config.MCP.In = &config.MCPToolCall{Tool: inTool}
	}

	if withPut {
		rt.Config.MCP.Out = &config.MCPToolCall{Tool: "create_issue"}
	}

	cfg := &config.Config{
		MCPServers:    []config.MCPServer{{Name: "linear", Endpoint: "https://mcp.linear.app/mcp"}},
		ResourceTypes: []config.ResourceType{rt},
		Resources:     []config.Resource{{Name: "eng-bugs", Type: "linear-issues", Source: map[string]any{"team": "ENG"}}},
	}

	return cfg, config.Step{Get: "eng-bugs"}
}

func TestMCPGetNodeContentFoldsInTool(t *testing.T) {
	t.Parallel()

	// Both share the same check: tool (irrelevant to GetNodeContent — see
	// TestMCPGetNodeContentUnaffectedWhenInUnset) and differ only in in:,
	// which GetNodeContent does fold in.
	cfgA, step := mcpResourceCfg("list_issues", "get_issue", false)
	cfgB, _ := mcpResourceCfg("list_issues", "fetch_issue", false)

	rtA, findErr := cfgA.FindResourceType("linear-issues")
	if findErr != nil {
		t.Fatalf("FindResourceType: %v", findErr)
	}

	rtB, findErr := cfgB.FindResourceType("linear-issues")
	if findErr != nil {
		t.Fatalf("FindResourceType: %v", findErr)
	}

	contentA, err := GetNodeContent(cfgA, step, *rtA, map[string]any{"team": "ENG"}, map[string]any{"id": "1"})
	if err != nil {
		t.Fatalf("GetNodeContent A: %v", err)
	}

	contentB, err := GetNodeContent(cfgB, step, *rtB, map[string]any{"team": "ENG"}, map[string]any{"id": "1"})
	if err != nil {
		t.Fatalf("GetNodeContent B: %v", err)
	}

	hashA, err := HashNode(NodeKindGet, contentA, "")
	if err != nil {
		t.Fatalf("HashNode A: %v", err)
	}

	hashB, err := HashNode(NodeKindGet, contentB, "")
	if err != nil {
		t.Fatalf("HashNode B: %v", err)
	}

	if hashA == hashB {
		t.Error("changing the mcp-backed resource type's in: tool should bust the get node's hash, but hashes matched")
	}
}

func TestMCPGetNodeContentUnaffectedWhenInUnset(t *testing.T) {
	t.Parallel()

	// Two resource types differing only in check: tool, but with in: unset in
	// both — GetNodeContent only folds in the *in* stage, so this proves the
	// documented asymmetry (check: is never hashed, matching the shell
	// backend's in_template-only behavior) rather than accidentally leaking.
	cfgA, step := mcpResourceCfg("list_issues", "", false)
	cfgB, _ := mcpResourceCfg("search_issues", "", false)

	rtA, err := cfgA.FindResourceType("linear-issues")
	if err != nil {
		t.Fatalf("FindResourceType: %v", err)
	}

	rtB, err := cfgB.FindResourceType("linear-issues")
	if err != nil {
		t.Fatalf("FindResourceType: %v", err)
	}

	contentA, err := GetNodeContent(cfgA, step, *rtA, map[string]any{"team": "ENG"}, map[string]any{"id": "1"})
	if err != nil {
		t.Fatalf("GetNodeContent A: %v", err)
	}

	contentB, err := GetNodeContent(cfgB, step, *rtB, map[string]any{"team": "ENG"}, map[string]any{"id": "1"})
	if err != nil {
		t.Fatalf("GetNodeContent B: %v", err)
	}

	hashA, err := HashNode(NodeKindGet, contentA, "")
	if err != nil {
		t.Fatalf("HashNode A: %v", err)
	}

	hashB, err := HashNode(NodeKindGet, contentB, "")
	if err != nil {
		t.Fatalf("HashNode B: %v", err)
	}

	if hashA != hashB {
		t.Error("check: tool changes must not affect the get node hash when in: is unset (mirrors the shell backend, where check: is never hashed)")
	}
}

func TestMCPPutNodeContentFoldsInTool(t *testing.T) {
	t.Parallel()

	cfg, _ := mcpResourceCfg("list_issues", "", true)
	step := config.Step{Put: "eng-bugs", Params: map[string]any{"title": "x"}}

	rt, err := cfg.FindResourceType("linear-issues")
	if err != nil {
		t.Fatalf("FindResourceType: %v", err)
	}

	contentWithOut, err := PutNodeContent(cfg, step, *rt, map[string]any{"team": "ENG"}, map[string]any{"title": "x"}, nil, false)
	if err != nil {
		t.Fatalf("PutNodeContent: %v", err)
	}

	if _, ok := contentWithOut["mcp_out_tool"]; !ok {
		t.Errorf("PutNodeContent = %+v, want an mcp_out_tool key", contentWithOut)
	}
}
