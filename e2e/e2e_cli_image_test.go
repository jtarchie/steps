package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2ECLIAgentImagePlacesTheToolsNotTheCLI is the seam issue #100 turns on,
// and the one no half of it can prove alone: `image:` on a CLI agent step
// containerizes the step's TOOLS, while the CLI process itself stays a
// subprocess of this one.
//
// One run answers both. The fake `claude` is installed on the HOST's PATH and
// the image has never heard of it, so the step running at all says the brain
// stayed here; the bridged run_shell answers with a marker only a file inside
// the image can produce, so the hands were over there. A design that
// containerized the CLI would fail the first half, and one that ignored
// image: for tools would fail the second.
func TestE2ECLIAgentImagePlacesTheToolsNotTheCLI(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "run_shell.json")

	claude := writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__run_shell") + "'",
		captureBridgeScript(captured, "run_shell",
			`{"command":"[ -f /etc/alpine-release ] && echo IN-THE-IMAGE"}`),
		"echo '" + cliResultEvent("ran the command", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: builder
  image: %s
  source:
    model: "@claude/sonnet"
  tools: [run_shell]

jobs:
- name: build
  plan:
  - agent: builder
    inputs: []
    messages:
      - Run the command.
`, dockerE2EImage))

	mustRun(t, path)

	if got := claude.invocations(t); got != 1 {
		t.Fatalf("the fake claude ran %d times, want 1 — the CLI runs on the host, which is the only place it exists", got)
	}

	if got := readFileString(t, captured); !strings.Contains(got, "IN-THE-IMAGE") {
		t.Errorf("run_shell answered %q, want the marker only /etc/alpine-release could produce — the tool did not run in the image", got)
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "builder")
}

// TestE2ECLIAgentImageWithNoNetworkStillReachesItsBridge is the load error
// this issue deleted, checked as a run rather than as an acceptance: cutting
// the container's egress narrows what the step's shell commands can reach and
// never touches the bridge, because the bridge is loopback on the host and the
// CLI dialling it was never inside the container.
func TestE2ECLIAgentImageWithNoNetworkStillReachesItsBridge(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "run_shell.json")

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__run_shell") + "'",
		captureBridgeScript(captured, "run_shell", `{"command":"echo SANDBOXED"}`),
		"echo '" + cliResultEvent("ran the command", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: sandboxed
  image: %s
  network: none
  source:
    model: "@claude/sonnet"
  tools: [run_shell]

jobs:
- name: build
  plan:
  - agent: sandboxed
    inputs: []
    messages:
      - Run the command.
`, dockerE2EImage))

	mustRun(t, path)

	if got := readFileString(t, captured); !strings.Contains(got, "SANDBOXED") {
		t.Errorf("run_shell answered %q, want the command's own output — a zero-egress container must still run the step's tools", got)
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "sandboxed")
}
