package config

import (
	"errors"
	"strings"
	"testing"
)

// The vocabulary two front ends share. It lived in internal/cli, where `steps mcp list` could read it and the web UI could not, so the page would have had to invent its own words for the same four facts.
func TestMCPServerRendersItsOwnRow(t *testing.T) {
	t.Parallel()

	stdio := MCPServer{Name: "gopls", Command: "gopls", Args: []string{"mcp"}, Cwd: "repo"}
	if got, want := stdio.Target(), "gopls mcp (cwd: repo)"; got != want {
		t.Errorf("Target(stdio) = %q, want %q", got, want)
	}

	if got, want := stdio.Transport(), "stdio"; got != want {
		t.Errorf("Transport(stdio) = %q, want %q", got, want)
	}

	http := MCPServer{Endpoint: "https://x/mcp"}
	if got, want := http.Target(), "https://x/mcp"; got != want {
		t.Errorf("Target(http) = %q, want %q", got, want)
	}

	if got, want := http.Transport(), "http"; got != want {
		t.Errorf("Transport(http) = %q, want %q", got, want)
	}

	// The env var NAME is what an operator checks; the value is never read here.
	bearer := MCPServer{Auth: MCPServerAuth{Type: "bearer", APIKeyEnv: "GITHUB_PAT"}}
	if got, want := bearer.AuthLabel(), "bearer $GITHUB_PAT"; got != want {
		t.Errorf("AuthLabel(bearer) = %q, want %q", got, want)
	}

	if got, want := (MCPServer{}).AuthLabel(), "none"; got != want {
		t.Errorf("AuthLabel(unset) = %q, want %q", got, want)
	}
}

// The cell that says what breaks while a server is disconnected, and the one that says nothing does.
//
//nolint:gosec // G101: APIKeyEnv is the NAME of a variable, which is the whole point — the value is never in the YAML and never here
func TestMCPUsersNamesTheConsumersOrSaysThereAreNone(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		MCPServers: []MCPServer{{Name: "linear", Endpoint: "https://x/mcp"}, {Name: "spare", Endpoint: "https://x/mcp"}},
		Agents: []Agent{{
			Name:   "triager",
			Source: AgentSource{Model: "openrouter/qwen/qwen3.7-flash", APIKeyEnv: "OPENROUTER_API_KEY"},
			Tools:  []ToolSpec{{MCP: "linear", MCPTool: "list_issues"}},
		}},
	}

	if got := cfg.MCPUsers("linear"); got != "agent triager" {
		t.Errorf("MCPUsers(linear) = %q, want the agent that grants it", got)
	}

	if got := cfg.MCPUsers("spare"); got != "(unused)" {
		t.Errorf("MCPUsers(spare) = %q, want it named as unused", got)
	}
}

// The row's first column is already the server's name, so the error's copies of it are noise in front of the part somebody has to act on.
func TestMCPStatusReasonDropsTheNameTheRowAlreadyCarries(t *testing.T) {
	t.Parallel()

	reason := MCPStatusReason("linear", errors.New(`mcp server "linear" is not authorized (run `+"`steps mcp login`"+`)`))
	if want := "is not authorized (run `steps mcp login`)"; reason != want {
		t.Errorf("MCPStatusReason = %q, want %q", reason, want)
	}

	if got := MCPStatusReason("test", errors.New(`mcp: connect to "test": dial tcp: refused`)); got != "dial tcp: refused" {
		t.Errorf("MCPStatusReason(connect) = %q", got)
	}

	// A multi-line error is a cell one line high; the first line is the one that says what happened.
	if got := MCPStatusReason("test", errors.New("first line\nsecond line")); got != "first line" {
		t.Errorf("MCPStatusReason kept more than the first line: %q", got)
	}
}

// The four shapes, and the two answers this package deliberately does not give.
// Not t.Parallel(): the environment is what is under test, and t.Setenv refuses a parallel test for exactly that reason.
//
//nolint:gosec // G101: every APIKeyEnv below is the NAME of a variable, which is the whole point — the value is never in the YAML and never here
func TestStaticStatusAnswersOnlyWhatNeedsNoRequest(t *testing.T) {
	t.Setenv("STEPS_TEST_MCP_PAT", "a-token")
	t.Setenv("STEPS_TEST_MCP_EMPTY", "")

	for _, test := range []struct {
		name      string
		server    MCPServer
		readiness MCPReadiness
		detail    string
	}{
		{
			name:      "a stdio command on PATH",
			server:    MCPServer{Command: "sh"},
			readiness: MCPReady,
			detail:    "on PATH",
		},
		{
			name:      "a stdio command that is not installed",
			server:    MCPServer{Command: "steps-no-such-mcp-server"},
			readiness: MCPMissing,
			detail:    `command "steps-no-such-mcp-server" not found on PATH`,
		},
		{
			// Resolved against an agent step's workspace, which exists only during a run — so this machine cannot answer, and a ✗ here would be a lie about a server that works.
			name:      "a stdio command with a per-step cwd",
			server:    MCPServer{Command: "steps-no-such-mcp-server", Cwd: "repo"},
			readiness: MCPUnknown,
			detail:    "not probed (cwd: repo resolves per step)",
		},
		{
			name:      "a bearer variable that is set",
			server:    MCPServer{Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "bearer", APIKeyEnv: "STEPS_TEST_MCP_PAT"}},
			readiness: MCPReady,
			detail:    "$STEPS_TEST_MCP_PAT is set",
		},
		{
			name:      "a bearer variable that is not set",
			server:    MCPServer{Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "bearer", APIKeyEnv: "STEPS_TEST_MCP_UNSET"}},
			readiness: MCPMissing,
			detail:    "$STEPS_TEST_MCP_UNSET is not set (auth.api_key_env)",
		},
		{
			// An empty value is as unusable as an absent one, and mcp.lookupBearerToken refuses both — a status that called this fine would send somebody looking at the endpoint.
			name:      "a bearer variable set to nothing",
			server:    MCPServer{Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "bearer", APIKeyEnv: "STEPS_TEST_MCP_EMPTY"}},
			readiness: MCPMissing,
			detail:    "$STEPS_TEST_MCP_EMPTY is not set (auth.api_key_env)",
		},
		{
			// The credential is a token file internal/mcp owns; this package may not import it, and guessing would be worse than saying who to ask.
			name:      "an oauth server",
			server:    MCPServer{Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "oauth"}},
			readiness: MCPUnknown,
			detail:    "needs a login",
		},
		{
			// Refused at load, but a status cell renders whatever it is handed and a blank one would read as fine.
			name:      "a bearer server with no api_key_env at all",
			server:    MCPServer{Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "bearer"}},
			readiness: MCPMissing,
			detail:    "auth.type: bearer requires api_key_env",
		},
		{
			name:      "an endpoint with no auth at all",
			server:    MCPServer{Endpoint: "https://x/mcp"},
			readiness: MCPReady,
			detail:    "no credential needed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := test.server.StaticStatus()
			if got.Readiness != test.readiness {
				t.Errorf("readiness = %v, want %v (%s)", got.Readiness, test.readiness, got.Detail)
			}

			if got.Detail != test.detail {
				t.Errorf("detail = %q, want %q", got.Detail, test.detail)
			}
		})
	}
}

// An unset credential is a fact about this machine that costs microseconds to check, and it was the one kind of MCP problem CheckEnvironment did not report — so it surfaced as a failed run, which is what the whole file exists to prevent.
//
//nolint:gosec // G101: APIKeyEnv is the NAME of a variable, which is the whole point — the value is never in the YAML and never here
func TestCheckEnvironmentReportsAnUnsetBearerCredential(t *testing.T) {
	cfg := &Config{MCPServers: []MCPServer{
		{Name: "github", Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "bearer", APIKeyEnv: "STEPS_TEST_MCP_UNSET"}},
		{Name: "public", Endpoint: "https://x/mcp"},
	}}

	problems := cfg.CheckEnvironment()
	if len(problems) != 1 {
		t.Fatalf("CheckEnvironment reported %v, want exactly the bearer server", problems)
	}

	if problems[0].Target != `mcp "github"` {
		t.Errorf("problem names %q, want the server", problems[0].Target)
	}

	if !strings.Contains(problems[0].Detail, "STEPS_TEST_MCP_UNSET") {
		t.Errorf("problem does not name the variable to set: %q", problems[0].Detail)
	}
}

// An oauth server is NOT a preflight problem: its credential is a token file this package cannot read, and refusing a set for one would make a daemon unable to hold the pipeline you are about to log in for.
func TestCheckEnvironmentLeavesOAuthToWhoeverHoldsTheToken(t *testing.T) {
	t.Parallel()

	cfg := &Config{MCPServers: []MCPServer{
		{Name: "linear", Endpoint: "https://x/mcp", Auth: MCPServerAuth{Type: "oauth"}},
	}}

	if problems := cfg.CheckEnvironment(); len(problems) != 0 {
		t.Errorf("CheckEnvironment refused an oauth server: %v", problems)
	}
}
