package mcpstatus

import (
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// The vocabulary two front ends share. It lived in internal/cli, where `steps mcp list` could read it and the web UI could not, so the page would have had to invent its own words for the same four facts.
func TestMCPServerRendersItsOwnRow(t *testing.T) {
	t.Parallel()

	stdio := config.MCPServer{Name: "gopls", Command: "gopls", Args: []string{"mcp"}, Cwd: "repo"}
	if got, want := Target(stdio), "gopls mcp (cwd: repo)"; got != want {
		t.Errorf("Target(stdio) = %q, want %q", got, want)
	}

	if got, want := Transport(stdio), "stdio"; got != want {
		t.Errorf("Transport(stdio) = %q, want %q", got, want)
	}

	http := config.MCPServer{Endpoint: "https://x/mcp"}
	if got, want := Target(http), "https://x/mcp"; got != want {
		t.Errorf("Target(http) = %q, want %q", got, want)
	}

	if got, want := Transport(http), "http"; got != want {
		t.Errorf("Transport(http) = %q, want %q", got, want)
	}

	// The env var NAME is what an operator checks; the value is never read here.
	bearer := config.MCPServer{Auth: config.MCPServerAuth{Type: "bearer", APIKeyEnv: "GITHUB_PAT"}}
	if got, want := AuthLabel(bearer), "bearer $GITHUB_PAT"; got != want {
		t.Errorf("AuthLabel(bearer) = %q, want %q", got, want)
	}

	if got, want := AuthLabel(config.MCPServer{}), "none"; got != want {
		t.Errorf("AuthLabel(unset) = %q, want %q", got, want)
	}
}

// The cell that says what breaks while a server is disconnected, and the one that says nothing does.
//
//nolint:gosec // G101: APIKeyEnv is the NAME of a variable, which is the whole point — the value is never in the YAML and never here
func TestMCPUsersNamesTheConsumersOrSaysThereAreNone(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		MCPServers: []config.MCPServer{{Name: "linear", Endpoint: "https://x/mcp"}, {Name: "spare", Endpoint: "https://x/mcp"}},
		Agents: []config.Agent{{
			Name:   "triager",
			Source: config.AgentSource{Model: "openrouter/qwen/qwen3.7-flash", APIKeyEnv: "OPENROUTER_API_KEY"},
			Tools:  []config.ToolSpec{{MCP: "linear", MCPTool: "list_issues"}},
		}},
	}

	if got := Users(cfg, "linear"); got != "agent triager" {
		t.Errorf("Users(linear) = %q, want the agent that grants it", got)
	}

	if got := Users(cfg, "spare"); got != "(unused)" {
		t.Errorf("Users(spare) = %q, want it named as unused", got)
	}
}

// The row's first column is already the server's name, so the error's copies of it are noise in front of the part somebody has to act on.
func TestMCPStatusReasonDropsTheNameTheRowAlreadyCarries(t *testing.T) {
	t.Parallel()

	reason := Reason("linear", errors.New(`mcp server "linear" is not authorized (run `+"`steps mcp login`"+`)`))
	if want := "is not authorized (run `steps mcp login`)"; reason != want {
		t.Errorf("Reason = %q, want %q", reason, want)
	}

	if got := Reason("test", errors.New(`mcp: connect to "test": dial tcp: refused`)); got != "dial tcp: refused" {
		t.Errorf("Reason(connect) = %q", got)
	}

	// A multi-line error is a cell one line high; the first line is the one that says what happened.
	if got := Reason("test", errors.New("first line\nsecond line")); got != "first line" {
		t.Errorf("Reason kept more than the first line: %q", got)
	}
}
