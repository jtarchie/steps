package e2e

// Not t.Parallel() anywhere here: runAndCapture swaps the process's streams, and the probe cache is process-wide.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
)

const e2eTrackerValue = "mcp-value-from-the-vault-9a3f"

// trackerVarOnlyInVault is a variable name nothing sets: the token these servers want exists in the vault alone.
const trackerVarOnlyInVault = "STEPS_TEST_MCP_VAR_ONLY_IN_VAULT"

// guardedMCP is an MCP server that answers only the bearer token it was started with, and remembers every Authorization it was shown.
type guardedMCP struct {
	URL string

	mu   sync.Mutex
	seen []string
}

func (g *guardedMCP) authorizations() []string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]string(nil), g.seen...)
}

func startGuardedMCP(t *testing.T) *guardedMCP {
	t.Helper()

	const token = e2eTrackerValue

	guarded := &guardedMCP{}
	srv := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "guarded", Version: "v0"}, nil)

	srv.AddTool(&sdkmcp.Tool{Name: "read_items", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			// The server echoes its caller's credential, as a careless one might: the model must be sent it masked.
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ITEM-1 for " + token}}}, nil
		})

	srv.AddTool(&sdkmcp.Tool{Name: "list_items", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *sdkmcp.CallToolRequest) (*sdkmcp.CallToolResult, error) {
			return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `[{"id":"ITEM-1"}]`}}}, nil
		})

	inner := sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return srv }, nil)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")

		guarded.mu.Lock()
		guarded.seen = append(guarded.seen, got)
		guarded.mu.Unlock()

		if got != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)

			return
		}

		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)

	guarded.URL = ts.URL

	return guarded
}

func mcpSecretPipeline(llm, mcp string) string {
	return `
mcp_servers:
- name: tracker
  endpoint: ` + mcp + `/mcp
  auth:
    type: bearer
    api_key_env: ` + trackerVarOnlyInVault + `
    secrets:
      ` + trackerVarOnlyInVault + `: ((ci:tracker_token))
agents:
- name: reader
  source: { model: openai/test-model, endpoint: ` + llm + `/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - mcp: tracker
    tool: read_items
jobs:
- name: read
  plan:
  - agent: reader
    inputs: []
    messages:
      - What items are there?
`
}

// The spine: the variable is absent from the environment, the server is sent the vault's value, and what the server echoes back is masked before the model or the database sees it.
func TestMCPBearerTokenComesFromASecretSource(t *testing.T) {
	for _, command := range []string{"run", "test"} {
		t.Run(command, func(t *testing.T) {
			isolatePreflightPins(t)
			t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

			_, path, config, vault := cliSecretsFixture(t, nil)
			name := cli.PipelineName(path)
			writeVault(t, vault, map[string]string{name + "/tracker_token": e2eTrackerValue + "\n"})

			server := startGuardedMCP(t)
			model := scopeLLM(t)

			writePipelineFile(t, path, mcpSecretPipeline(model.URL, server.URL))

			printed, err := runAndCapture(t, command, "--config", config, path)
			if err != nil {
				t.Fatalf("steps %s --config: %v\n%s", command, err, printed)
			}

			seen := server.authorizations()
			if len(seen) == 0 {
				t.Fatal("the MCP server was never reached, so its credential proves nothing")
			}

			for i, got := range seen {
				if got != "Bearer "+e2eTrackerValue {
					t.Errorf("request %d carried Authorization %q, want the vault's value as its bearer token", i+1, got)
				}
			}

			var results []string
			for _, request := range model.allRequests() {
				results = append(results, request.toolResults()...)
			}

			joined := strings.Join(results, "\n")
			if !strings.Contains(joined, "ITEM-1 for ***") {
				t.Errorf("the tool result the model was sent = %q, want the server's answer with the token masked", joined)
			}

			assertNotInDatabase(t, string(cli.StatePath(path, "")), e2eTrackerValue)
		})
	}
}

// Two pipelines hold the same server definition and resolve its token to different credentials: the one whose token is refused must not condemn the other from the probe cache.
func TestAnMCPProbeVerdictIsNotSharedAcrossPipelinesWithDifferentTokens(t *testing.T) {
	isolatePreflightPins(t)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	dir, _, config, vault := cliSecretsFixture(t, nil)
	writeVault(t, vault, map[string]string{"wrong/tracker_token": "not-the-token-0000", "right/tracker_token": e2eTrackerValue})

	server := startGuardedMCP(t)
	model := scopeLLM(t)

	wrong := filepath.Join(dir, "wrong.yml")
	right := filepath.Join(dir, "right.yml")
	writePipelineFile(t, wrong, mcpSecretPipeline(model.URL, server.URL))
	writePipelineFile(t, right, mcpSecretPipeline(model.URL, server.URL))

	printed, err := runAndCapture(t, "run", "--config", config, wrong)
	if err == nil {
		t.Fatalf("the pipeline whose token the server refuses ran clean:\n%s", printed)
	}

	printed, err = runAndCapture(t, "run", "--config", config, right)
	if err != nil {
		t.Fatalf("the pipeline with the right token was refused on its neighbour's verdict: %v\n%s", err, printed)
	}
}

// A secrets: map on a server is held to the one key api_key_env names, on a bearer server only.
func TestMCPAuthSecretsAreRefusedWhereNothingWouldReadThem(t *testing.T) {
	for _, tc := range []struct{ name, auth, want string }{
		{"a key api_key_env does not name", "{ type: bearer, api_key_env: TOKEN, secrets: { OTHER: ((ci:x)) } }", "api_key_env TOKEN is not a key of its secrets: map"},
		{"a second key", "{ type: bearer, api_key_env: TOKEN, secrets: { TOKEN: ((ci:x)), OTHER: ((ci:y)) } }", "nothing would read OTHER"},
		{"an oauth server", "{ type: oauth, secrets: { TOKEN: ((ci:x)) } }", "secrets: applies to auth.type: bearer"},
		{"a literal", "{ type: bearer, api_key_env: TOKEN, secrets: { TOKEN: hunter2 } }", "((source:name))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := pipelinePath(t, t.TempDir())
			writePipelineFile(t, path, `
mcp_servers:
- name: tracker
  endpoint: https://example.com/mcp
  auth: `+tc.auth+`
jobs:
- name: noop
  plan:
  - task: noop
    run: "true"
`)

			printed, err := runAndCapture(t, "validate", "--syntax-only", path)
			if err == nil || !strings.Contains(printed, tc.want) {
				t.Errorf("validate = %v, want a refusal containing %q\n%s", err, tc.want, printed)
			}
		})
	}
}

func mcpResourceSecretPipeline(mcp string) string {
	return `
mcp_servers:
- name: tracker
  endpoint: ` + mcp + `/mcp
  auth:
    type: bearer
    api_key_env: ` + trackerVarOnlyInVault + `
    secrets:
      ` + trackerVarOnlyInVault + `: ((ci:tracker_token))
resource_types:
- name: items
  config:
    mcp:
      server: tracker
      check:
        tool: list_items
resources:
- name: things
  type: items
  source: { team: ENG }
jobs:
- name: react
  plan:
  - get: things
  - task: record
    inputs: [things]
    run: cat things/version.json
`
}

// The resource half of the seam: a type backed by the server checks with the vault's token, and its probe verdict is the pipeline's own too.
func TestAnMCPBackedResourceChecksWithASecretBackedToken(t *testing.T) {
	isolatePreflightPins(t)

	dir, _, config, vault := cliSecretsFixture(t, nil)
	writeVault(t, vault, map[string]string{"wrong/tracker_token": "not-the-token-0000", "right/tracker_token": e2eTrackerValue})

	server := startGuardedMCP(t)

	wrong := filepath.Join(dir, "wrong.yml")
	right := filepath.Join(dir, "right.yml")
	writePipelineFile(t, wrong, mcpResourceSecretPipeline(server.URL))
	writePipelineFile(t, right, mcpResourceSecretPipeline(server.URL))

	printed, err := runAndCapture(t, "run", "--config", config, wrong)
	if err == nil {
		t.Fatalf("the pipeline whose token the server refuses ran clean:\n%s", printed)
	}

	printed, err = runAndCapture(t, "run", "--config", config, right)
	if err != nil {
		t.Fatalf("the pipeline with the right token was refused: %v\n%s", err, printed)
	}

	if !strings.Contains(printed, "ITEM-1") {
		t.Errorf("the get did not fetch the version the server lists:\n%s", printed)
	}

	assertNotInDatabase(t, string(cli.StatePath(right, "")), e2eTrackerValue)
}

// steps mcp list reports rather than refuses: with the daemon's file the server is probed with its token, and without it the row says what is missing.
func TestMCPListProbesASecretBackedServerWithConfig(t *testing.T) {
	_, path, config, vault := cliSecretsFixture(t, nil)
	writeVault(t, vault, map[string]string{cli.PipelineName(path) + "/tracker_token": e2eTrackerValue})

	server := startGuardedMCP(t)
	writePipelineFile(t, path, mcpSecretPipeline("http://127.0.0.1:1", server.URL))

	printed, err := runAndCapture(t, "mcp", "list", path, "--config", config)
	if err != nil || !strings.Contains(printed, "✓ 2 tools") {
		t.Errorf("mcp list --config = %v, want the server probed with its token\n%s", err, printed)
	}

	printed, err = runAndCapture(t, "mcp", "tools", path, "tracker", "--config", config)
	if err != nil || !strings.Contains(printed, "read_items") {
		t.Errorf("mcp tools --config = %v, want the server's tools\n%s", err, printed)
	}

	printed, err = runAndCapture(t, "mcp", "list", path)
	if err != nil || !strings.Contains(printed, "pass --config") {
		t.Errorf("mcp list without --config = %v, want a row saying the token needs a secret source\n%s", err, printed)
	}

	if strings.Contains(printed, e2eTrackerValue) {
		t.Errorf("mcp list printed the token:\n%s", printed)
	}
}

// validate --live resolves the token the way the daemon would, names its path, and probes the server with it.
func TestValidateLiveResolvesAnMCPTokenAndNamesItsPath(t *testing.T) {
	isolatePreflightPins(t)
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	_, path, config, vault := cliSecretsFixture(t, nil)
	name := cli.PipelineName(path)
	writeVault(t, vault, map[string]string{name + "/tracker_token": e2eTrackerValue})

	server := startGuardedMCP(t)
	model := scopeLLM(t)
	writePipelineFile(t, path, mcpSecretPipeline(model.URL, server.URL))

	printed, err := runAndCapture(t, "validate", "--live", "--config", config, path)
	if err != nil {
		t.Fatalf("validate --live --config: %v\n%s", err, printed)
	}

	want := `secret ((ci:tracker_token)) for job "read" is at ` + filepath.Join(vault, name, "tracker_token")
	if !strings.Contains(printed, want) {
		t.Errorf("validate --live did not name the path that answered, want %q\n%s", want, printed)
	}

	if len(server.authorizations()) == 0 {
		t.Error("validate --live never probed the server, so it vouched for nothing")
	}

	if strings.Contains(printed, e2eTrackerValue) {
		t.Errorf("validate --live printed the token:\n%s", printed)
	}
}

// An export answers an MCP grant with no run, so a server whose token is a reference is refused there at load.
func TestAnExportRefusesAServerWithASecretBackedToken(t *testing.T) {
	path := pipelinePath(t, t.TempDir())
	writePipelineFile(t, path, `
mcp_servers:
- name: tracker
  endpoint: https://example.com/mcp
  auth: { type: bearer, api_key_env: TOKEN, secrets: { TOKEN: ((ci:x)) } }
mcp_exports:
- name: support
  access: '"acme/support" in Groups'
  tools:
  - mcp: tracker
    tool: read_items
jobs:
- name: noop
  plan:
  - task: noop
    run: "true"
`)

	printed, err := runAndCapture(t, "validate", "--syntax-only", path)
	if err == nil || !strings.Contains(printed, "an export has no run to resolve them in") {
		t.Errorf("validate = %v, want the export refused\n%s", err, printed)
	}
}

// A token that could not be FETCHED says nothing about the server: once the vault has it, the next run must not be refused on the remembered failure.
func TestAFailedMCPTokenLookupIsNotRememberedAsADeadServer(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pipeline func(llm, mcp string) string
	}{
		{"an agent's grant", mcpSecretPipeline},
		{"a resource type's backend", func(_, mcp string) string { return mcpResourceSecretPipeline(mcp) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatePreflightPins(t)
			t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

			_, path, config, vault := cliSecretsFixture(t, nil)

			server := startGuardedMCP(t)
			model := scopeLLM(t)
			writePipelineFile(t, path, tc.pipeline(model.URL, server.URL))

			printed, err := runAndCapture(t, "run", "--config", config, path)
			if err == nil {
				t.Fatalf("a run whose token is in no vault ran clean:\n%s", printed)
			}

			writeVault(t, vault, map[string]string{cli.PipelineName(path) + "/tracker_token": e2eTrackerValue})

			printed, err = runAndCapture(t, "run", "--config", config, path)
			if err != nil {
				t.Fatalf("the run after the vault was filled was refused on the earlier lookup failure: %v\n%s", err, printed)
			}
		})
	}
}
