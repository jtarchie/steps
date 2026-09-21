package e2e

// `tags:` on an agent was refused because only its run_shell would travel: its file tools ran in the steps process against the orchestrator's copy of the tree, so `write_file` and `run_shell` addressed two different filesystems and both reported success, with nothing the model could read to tell. This is that bug made concrete, which is why it is the first test.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlacedAgentSharesOneTreeWithItsShell places an agent on a local: worker and makes its file tools and its shell take turns on one file. Docker is required and the issue's "needs no docker" framing could not hold: `tags:` without `image:` stays refused, so the only placed agent there is runs its tools in a container on the worker.
func TestPlacedAgentSharesOneTreeWithItsShell(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	published := filepath.Join(dir, "published.txt")

	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{
			"path":    "data/AGENT.txt",
			"content": "from-the-tool\n",
		}),
		callsTool("run_shell", map[string]any{
			"command": "cat data/AGENT.txt && echo from-the-shell >> data/AGENT.txt && echo worker-only > SCRATCH.txt",
		}),
		callsTool("read_file", map[string]any{"path": "data/AGENT.txt"}),
		// SCRATCH.txt sits OUTSIDE outputs:, so the venue never fetches it: it exists on the worker and nowhere else. Reading it is the only assertion here that a host-side file tool could not also satisfy.
		callsTool("read_file", map[string]any{"path": "SCRATCH.txt"}),
		says("the tool and the shell worked on one tree"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: hand
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file, run_shell]

jobs:
- name: place
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - agent: hand
    tags: [box]
    inputs: [data]
    outputs: [data]
    messages:
      - Take turns with the shell on data/AGENT.txt.

  - task: publish
    inputs: [data]
    run: cp data/AGENT.txt %[3]s
`, fake.URL, dockerE2EImage, published))

	mustRun(t, path, "--worker", "box=local:")

	// Placed, these are the two halves that used to address different machines. A call made in request N is answered in request N+1, so the shell's own result is request 3.
	shell := lastToolResult(t, fake.request(3))
	if !strings.Contains(shell, "from-the-tool") {
		t.Errorf("run_shell result = %q, want the file write_file made — the tool and the shell are on different trees", shell)
	}

	read := lastToolResult(t, fake.request(4))
	if !strings.Contains(read, "from-the-shell") {
		t.Errorf("read_file result = %q, want the line the containerized shell appended", read)
	}

	// The discriminator: this file was never fetched home, so a file tool still running in this process cannot see it. Without it the rest of this test passes even when the tools never leave the orchestrator, because the tree is uploaded lazily on the first COMMAND — a host-side write lands before the upload, and the declared outputs come back afterwards.
	scratch := lastToolResult(t, fake.request(5))
	if !strings.Contains(scratch, "worker-only") {
		t.Errorf("read_file of SCRATCH.txt = %q, want the file the worker's container holds — the file tools are still running on the orchestrator", scratch)
	}

	got := readFileString(t, published)
	for _, want := range []string{"from-the-tool", "from-the-shell"} {
		if !strings.Contains(got, want) {
			t.Errorf("published = %q, want %q — the placed agent's tree did not come home", got, want)
		}
	}

	assertSucceeded(t, storeNodes(t, path), "agent", "hand")
}

// TestPlacedAgentWithDirStillFetchesItsOutputs covers the seam between dir: and the venue. An agent's runner is pointed at its dir:, while outputs: are named relative to the whole step directory — so sending only the subdirectory would upload a tree missing the agent's other inputs and then fetch its outputs from a path that does not exist, and the step would come home empty while reporting success.
func TestPlacedAgentWithDirStillFetchesItsOutputs(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()
	published := filepath.Join(dir, "published-dir.txt")

	fake := newFakeLLM(t,
		callsTool("write_file", map[string]any{"path": "INNER.txt", "content": "written-in-the-subdir\n"}),
		callsTool("run_shell", map[string]any{"command": "cat INNER.txt && pwd > ../out/where.txt && cat INNER.txt >> ../out/where.txt"}),
		says("worked below the step directory"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: hand
  image: %[2]s
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file, run_shell]

jobs:
- name: place
  plan:
  - task: prepare
    outputs: [code, out]
    run: echo seed > code/seed.txt

  - agent: hand
    tags: [box]
    dir: code
    inputs: [code]
    outputs: [out]
    messages:
      - Work inside the code directory.

  - task: publish
    inputs: [out]
    run: cp out/where.txt %[3]s
`, fake.URL, dockerE2EImage, published))

	mustRun(t, path, "--worker", "box=local:")

	// The model's file tool wrote into the subdirectory, and the shell that read it was in the same place.
	shell := lastToolResult(t, fake.request(3))
	if !strings.Contains(shell, "written-in-the-subdir") {
		t.Errorf("run_shell result = %q, want the file write_file made in dir:", shell)
	}

	got := readFileString(t, published)
	if !strings.Contains(got, "written-in-the-subdir") {
		t.Errorf("published = %q, want the output fetched from the step directory rather than from dir:", got)
	}

	if !strings.Contains(got, "/code") {
		t.Errorf("published = %q, want the container's working directory to have been the subdirectory", got)
	}
}

// TestPlacedAgentUnderNetworkNoneStillReadsItsTree separates two things that now share a container: the file tools reach the tree through the image's userland, which needs no network, while run_shell's egress stays cut. Routing the tools through the same container would be a poor trade if it quietly re-opened the fence, or if it broke under it.
func TestPlacedAgentUnderNetworkNoneStillReadsItsTree(t *testing.T) {
	requireDockerE2E(t)

	dir := t.TempDir()

	fake := newFakeLLM(t,
		// The note is written by the SHELL, at the tree root rather than under outputs:, so it exists on the worker and is never fetched home. A search that finds it can only have run in the container.
		callsTool("run_shell", map[string]any{"command": "echo fenced-note > NOTE.txt; wget -q -T2 -O- http://example.com || echo no-egress"}),
		callsTool("search_files", map[string]any{"pattern": `fenc\w+`, "output_mode": "content"}),
		says("read the tree without a network"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fenced
  image: %[2]s
  network: none
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, search_files, run_shell]

jobs:
- name: place
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - agent: fenced
    tags: [box]
    inputs: [data]
    outputs: [data]
    messages:
      - Work with no network.
`, fake.URL, dockerE2EImage))

	mustRun(t, path, "--worker", "box=local:")

	egress := lastToolResult(t, fake.request(2))
	if !strings.Contains(egress, "no-egress") {
		t.Errorf("run_shell result = %q, want the fence to have held", egress)
	}

	// The search walked the container with find and grep, behind a network fence, and found a file that exists only there. Whether the rendered pattern means the same thing in every image is the parity test's question, not this one's.
	found := lastToolResult(t, fake.request(3))
	if !strings.Contains(found, "fenced-note") {
		t.Errorf("search_files result = %q, want the line the fenced container's shell wrote", found)
	}
}
