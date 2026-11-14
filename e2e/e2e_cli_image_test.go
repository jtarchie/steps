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

// TestE2ECLIAgentImageWritesThenReadsThroughVolumes is security-audit's shape on the default path: a CLI agent with no network writes a file, a custom tool reads it in the container, and the file arrives on this machine for the next step (steps#246).
func TestE2ECLIAgentImageWritesThenReadsThroughVolumes(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "check.json")
	home := filepath.Join(dir, "home.txt")

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__write_file", "mcp__steps__check") + "'",
		captureBridgeScript(filepath.Join(t.TempDir(), "write.json"), "write_file", `{"path":"report/notes.txt","content":"written\n"}`),
		captureBridgeScript(captured, "check", `{}`),
		"echo '" + cliResultEvent("wrote and checked", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: recorder
  image: %s
  network: none
  source:
    model: "@claude/sonnet"
  tools:
  - write_file
  - name: check
    description: Read the report back.
    run: pwd && cat report/notes.txt

jobs:
- name: build
  plan:
  - agent: recorder
    outputs: [report]
    messages:
      - Write the report.
  - task: publish
    inputs: [report]
    run: cp report/notes.txt %s
`, dockerE2EImage, home))

	mustRun(t, path)

	if got := readFileString(t, captured); !strings.Contains(got, buildDir("recorder")) || !strings.Contains(got, "written") {
		t.Errorf("check answered %q, want the file write_file made, read in the volume workdir", got)
	}

	if got := readFileString(t, home); got != "written\n" {
		t.Errorf("the next step read %q, want what the agent wrote", got)
	}
}

// TestE2ECLIAgentImageWithTagsPlacesItsToolsOnTheWorker is steps#241: a CLI agent that sets image: may take tags:, by the rule a hosted agent follows — the conversation stays here, the tools are placed. One run crosses every seam the issue lists: dir: is where the tools work on the worker, write_file and a custom tool share that tree, the declared file is checked after it comes home rather than against this machine's copy, and settings: project is still accepted.
func TestE2ECLIAgentImageWithTagsPlacesItsToolsOnTheWorker(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "check.json")
	home := filepath.Join(dir, "home.txt")

	claude := writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__write_file", "mcp__steps__check") + "'",
		captureBridgeScript(filepath.Join(t.TempDir(), "write.json"), "write_file", `{"path":"notes.txt","content":"written\n"}`),
		captureBridgeScript(captured, "check", `{}`),
		"echo '" + cliResultEvent("wrote and checked", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: recorder
  image: %s
  settings: project
  source:
    model: "@claude/sonnet"
  tools:
  - write_file
  - name: check
    description: Read the notes back.
    run: echo "on=$STEPS_WORKER in=$(pwd)" && cat notes.txt seeded.txt

jobs:
- name: build
  plan:
  - task: seed
    outputs: [report]
    run: echo seeded > report/seeded.txt
  - agent: recorder
    tags: [box]
    dir: report
    inputs: [report]
    outputs: [report]
    assert:
      files: [report/notes.txt]
    messages:
      - Write the notes.
  - task: publish
    inputs: [report]
    run: cp report/notes.txt %s
`, dockerE2EImage, home))

	mustRun(t, path, "--worker", "box=local:")

	if got := claude.invocations(t); got != 1 {
		t.Fatalf("the fake claude ran %d times, want 1 — the CLI stays on this machine", got)
	}

	got := readFileString(t, captured)
	for _, want := range []string{"on=box", "in=" + buildDir("recorder") + "/report", "written", "seeded"} {
		if !strings.Contains(got, want) {
			t.Errorf("check answered %q, want %q: the tools did not run in dir: on the worker over one tree", got, want)
		}
	}

	if got := readFileString(t, home); got != "written\n" {
		t.Errorf("the file the agent wrote on the worker came home as %q", got)
	}

	if placed := placementNamed(t, runPlacements(t, path), "recorder"); placed.Tag != "box" {
		t.Errorf("placement = %+v, want the step recorded on the worker tagged box", placed)
	}
}
