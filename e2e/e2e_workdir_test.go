package e2e

// steps#247: a container step works in a path that belongs to the container and is laid out as Concourse lays it out — /tmp/build/<8 hex of the step's name> for a task, /tmp/build/get and /tmp/build/put for a resource — the same on this machine's daemon and on a worker, and naming nothing on the host.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// buildDir is Concourse's rule, restated rather than imported so the test fails if the rule moves.
func buildDir(step string) string {
	sum := sha256.Sum256([]byte(step))

	return "/tmp/build/" + hex.EncodeToString(sum[:4])
}

func TestAContainerStepWorksInConcoursesLayout(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "out")

	err := os.Mkdir(out, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: here
  image: %[1]s
  config:
    check: printf '[{"ref":"v1"}]'
    in: pwd > where.txt
    out: |
      echo "put ran in $(pwd) holding $(ls)" >&2
      printf '{"ref":"v2"}'

resources:
- name: repo
  type: here
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: look
    image: %[1]s
    inputs: [repo]
    outputs: [look]
    caches: [{path: cache}]
    run: |
      pwd > look/task.txt
      ls -d "$PWD"/repo "$PWD"/cache >> look/task.txt
      env > look/env.txt
  - task: tagged
    image: %[1]s
    tags: [box]
    inputs: []
    outputs: [tagged]
    run: pwd > tagged/task.txt
  - put: repo
    inputs: [look]
  - task: publish
    inputs: [repo, look, tagged]
    run: cp repo/where.txt %[2]s/get.txt && cp look/* %[2]s/ && cp tagged/task.txt %[2]s/tagged.txt
`, dockerE2EImage, out))

	printed, err := runAndCapture(t, "run", "--no-preflight", "--worker", "box=local:", path)
	if err != nil {
		t.Fatalf("run: %v\n%s", err, printed)
	}

	look := buildDir("look")

	for file, want := range map[string]string{
		"get.txt":    "/tmp/build/get\n",
		"task.txt":   look + "\n" + look + "/cache\n" + look + "/repo\n",
		"tagged.txt": buildDir("tagged") + "\n",
	} {
		if got := readFileString(t, filepath.Join(out, file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}

	if !strings.Contains(printed, "put ran in /tmp/build/put holding look") {
		t.Errorf("the put did not run in /tmp/build/put with its input beneath it:\n%s", printed)
	}

	home, _ := os.UserHomeDir()

	for _, hostPath := range []string{dir, home, os.TempDir()} {
		if env := readFileString(t, filepath.Join(out, "env.txt")); hostPath != "" && strings.Contains(env, hostPath) {
			t.Errorf("the container's environment names the host path %s:\n%s", hostPath, env)
		}
	}
}

var spillPointer = regexp.MustCompile(`saved to (\S+)\.`)

// TestAContainerizedAgentIsToldNoHostPath: the model's system message, every tool result and the spill pointer for an oversized output name the container's layout, and the pointer it is handed still reads back.
func TestAContainerizedAgentIsToldNoHostPath(t *testing.T) {
	requireDockerE2E(t)
	dir := t.TempDir()

	fake := newRoutedFakeLLM(t, spillThenReadBack)

	path := writePipeline(t, dir, fmt.Sprintf(`
agents:
- name: hand
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, run_shell]

jobs:
- name: build
  plan:
  - agent: hand
    inputs: []
    messages: [go]
`, fake.URL, dockerE2EImage))

	mustRun(t, path, "--no-preflight")

	requests := fake.allRequests()
	if len(requests) != 3 {
		t.Fatalf("the conversation took %d requests, want 3", len(requests))
	}

	last := requests[len(requests)-1]
	workdir := buildDir("hand")

	if !strings.Contains(last.systemMessage(), workdir) {
		t.Errorf("the system message does not name the container's working directory %s:\n%s", workdir, last.systemMessage())
	}

	results := last.toolResults()

	if !strings.Contains(results[0], "saved to "+workdir+"/.steps-agent-out/") {
		t.Errorf("the spill pointer is not under the container's working directory:\n%.400s", results[0])
	}

	if !strings.Contains(results[1], "1\\n2") && !strings.Contains(results[1], "1\n2") {
		t.Errorf("reading the spilled output back by the name it was given found nothing:\n%.400s", results[1])
	}

	assertNamesNoHostPath(t, last.systemMessage()+strings.Join(results, "\n"), dir)
}

// spillThenReadBack prints more than a tool result holds, then reads the saved file back by whatever name the spill pointer gave it.
func spillThenReadBack(req capturedRequest) turn {
	results := req.toolResults()

	switch len(results) {
	case 0:
		return callsTool("run_shell", map[string]any{"command": "pwd; seq 1 20000"})
	case 1:
		found := spillPointer.FindStringSubmatch(results[0])
		if found == nil {
			return says("no spill pointer in: " + results[0])
		}

		return callsTool("read_file", map[string]any{"path": found[1], "start_line": 2, "end_line": 3})
	default:
		return says("done")
	}
}

func assertNamesNoHostPath(t *testing.T, said, dir string) {
	t.Helper()

	home, _ := os.UserHomeDir()

	for _, hostPath := range []string{dir, home, os.TempDir()} {
		if hostPath != "" && strings.Contains(said, hostPath) {
			t.Errorf("the model was told the host path %s", hostPath)
		}
	}
}

// TestAContainerizedCLIAgentIsHandedNoHostPath is #247's line for a CLI agent: everything steps hands the CLI — its arguments, the system prompt it appends, the message on stdin, and a bridged tool's result — names the container's working directory and nothing on this machine. What the CLI says to its own model about the directory its process started in is the CLI's, and is documented in docs/infra.md.
func TestAContainerizedCLIAgentIsHandedNoHostPath(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "run_shell.json")

	claude := writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__run_shell") + "'",
		captureBridgeScript(captured, "run_shell", `{"command":"pwd; seq 1 20000"}`),
		"echo '" + cliResultEvent("ran it", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: shell
  image: %s
  source:
    model: "@claude/sonnet"
  tools: [run_shell]

jobs:
- name: build
  plan:
  - agent: shell
    inputs: []
    messages:
      - Run it.
`, dockerE2EImage))

	mustRun(t, path)

	argv, prompt, result := claude.argv(t, 1), claude.prompt(t, 1), readFileString(t, captured)

	if !strings.Contains(argv, buildDir("shell")) {
		t.Errorf("the CLI's appended system prompt does not name the container's working directory %s:\n%s", buildDir("shell"), argv)
	}

	if !strings.Contains(result, buildDir("shell")+"/.steps-agent-out/") {
		t.Errorf("the bridged result's spill pointer is not under the container's working directory:\n%.400s", result)
	}

	// The --mcp-config file is where the CLI process finds the bridge, read by the process and never shown to its model.
	shown := slices.DeleteFunc(strings.Split(argv, "|"), func(arg string) bool { return strings.Contains(arg, "steps-cli-mcp-") })

	assertNamesNoHostPath(t, strings.Join(shown, "|")+prompt+result, dir)
}
