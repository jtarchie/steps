package agent

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/jtarchie/steps/internal/config"
)

// TestASubAgentHandsBackItsMCPConnection: the child's MCP sessions are opened when the parent's tools are built, so only the closer returned here can ever end them.
func TestASubAgentHandsBackItsMCPConnection(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{
		MCPServers: []config.MCPServer{srv.server()},
		Agents: []config.Agent{{
			Name: "extra", Source: config.AgentSource{Model: "lmstudio/qwen"},
			Tools: []config.ToolSpec{{MCP: "test", MCPTool: "search_issues"}},
		}},
	}

	_, _, closer, err := buildSubAgentTool(context.Background(), cfg, config.ToolSpec{Agent: "extra"})
	if err != nil {
		t.Fatal(err)
	}

	if closer == nil {
		t.Fatal("no closer for a sub-agent holding an MCP connection")
	}

	err = closer.Close()
	if err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestSubAgentLogsTheDepthItRunsAt: the start/finish records are how a reader of slog output tells a delegated conversation from its parent.
func TestSubAgentLogsTheDepthItRunsAt(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	child := newTestSubAgent(t, &fakeLLM{responses: []*model.LLMResponse{textResponse("done")}})
	env := toolEnv{dir: t.TempDir(), transcript: &transcriptRecorder{}}

	child.run(context.Background(), map[string]any{"request": "go"}, env)

	for _, record := range []string{"agent.subagent_start", "agent.subagent_finish"} {
		var line string

		for l := range strings.Lines(buf.String()) {
			if strings.Contains(l, record) {
				line = l
			}
		}

		if !strings.Contains(line, " depth=1 ") {
			t.Errorf("%s does not log depth=1: %q", record, line)
		}
	}
}
