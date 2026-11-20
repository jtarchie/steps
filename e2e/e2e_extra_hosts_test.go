package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// TestEndToEndExtraHostsResolveInsideEveryContainer runs the three entries extra_hosts: lives on — a resource_type (list form), a task (map form) and an agent — each in an image, and reads the name back from inside the container each one starts (#291).
func TestEndToEndExtraHostsResolveInsideEveryContainer(t *testing.T) {
	t.Parallel()
	dockerSlot(t)

	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "getent hosts agenthost.test"}),
		says("resolved"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: aliased
  image: %[2]s
  extra_hosts: ["typehost.test:10.7.8.9"]
  config:
    check: printf '[{"ip":"%%s"}]' "$(getent hosts typehost.test | cut -d' ' -f1)"
    in: getent hosts typehost.test > ip.txt

resources:
- name: seen
  type: aliased
  source: {}

tasks:
- name: lookup
  image: %[2]s
  extra_hosts:
    myhost.test: 10.1.2.3
  inputs: [seen]
  run: echo "resolved $(getent hosts myhost.test | cut -d' ' -f1)/$(cut -d' ' -f1 seen/ip.txt)"

agents:
- name: prober
  image: %[2]s
  extra_hosts: {agenthost.test: 10.4.5.6}
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: resolve
  plan:
  - get: seen
  - task: lookup
    assert:
      stdout: resolved 10.1.2.3/10.7.8.9
  - agent: prober
    inputs: []
    messages:
      - Resolve the name.
`, fake.URL, dockerE2EImage))

	mustRun(t, path)

	assertSucceeded(t, storeNodes(t, path), "task", "lookup")

	if got := lastToolResult(t, fake.request(2)); !strings.Contains(got, "10.4.5.6") {
		t.Errorf("run_shell result = %q, want agenthost.test resolved to 10.4.5.6 inside the agent's container", got)
	}
}
