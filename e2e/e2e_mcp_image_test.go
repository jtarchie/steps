package e2e

// An mcp_servers: entry with image: runs its stdio command in a container (#263). The server is a busybox sh loop answering just enough of MCP, and its one tool reads /etc/alpine-release, a file this machine does not have.

import (
	"regexp"
	"testing"
)

const shellMCPServer = `
      while IFS= read -r line; do
        id=$(printf '%s' "$line" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')
        [ -n "$id" ] || continue
        case "$line" in
          *'"initialize"'*)
            version=$(printf '%s' "$line" | sed -n 's/.*"protocolVersion":"\([^"]*\)".*/\1/p')
            printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"%s","capabilities":{"tools":{}},"serverInfo":{"name":"boxed","version":"1"}}}\n' "$id" "$version" ;;
          *'"tools/list"'*)
            printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"release","description":"the os release","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
          *'"tools/call"'*)
            printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"alpine %s"}]}}\n' "$id" "$(cat /etc/alpine-release)" ;;
          *)
            printf '{"jsonrpc":"2.0","id":%s,"result":{}}\n' "$id" ;;
        esac
      done
`

func TestMCPServerImageRunsTheStdioServerInAContainer(t *testing.T) {
	t.Parallel()
	requireDockerE2E(t)
	dockerSlot(t)

	fake := newFakeLLM(t, callsTool("boxed__release", map[string]any{}), says("done"))

	path := writePipeline(t, t.TempDir(), `
defaults:
  preflight:
    disabled: true

mcp_servers:
- name: boxed
  image: alpine:3
  command: sh
  args:
    - -c
    - |`+shellMCPServer+`
agents:
- name: asker
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [{mcp: boxed}]

jobs:
- name: ask
  plan:
  - agent: asker
    inputs: []
    messages:
      - Which release is this?
`)
	mustRun(t, "run", path)

	if got := lastToolResult(t, fake.request(2)); !regexp.MustCompile(`alpine \d+\.\d+`).MatchString(got) {
		t.Errorf("tool result = %q, want the container's alpine release", got)
	}
}
