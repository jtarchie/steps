package e2e

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

func requireSandbox(t *testing.T) {
	t.Helper()

	if runtime.GOOS != "darwin" {
		t.Skip("sandbox: is implemented with sandbox-exec, darwin only")
	}
}

// outsideDir is a directory the step can name but must not write: the test's own temp dir, which is neither the step's workspace nor its TMPDIR.
func outsideDir(t *testing.T) string {
	t.Helper()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return dir
}

// The task's script states the policy as exit codes, so a run that passes is the sandbox holding on every point, and the Go side checks the two things the script cannot see from inside: nothing landed outside, and nothing reached the server.
func TestSandboxedTaskWritesOnlyItsWorkspace(t *testing.T) {
	requireSandbox(t)

	dir := t.TempDir()
	outside := outsideDir(t)

	var hits atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()

	systemTmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	path := writePipeline(t, dir, fmt.Sprintf(`
tasks:
- name: probe
  sandbox: true
  network: none
  outputs: [out]
  run: |
    touch out/inside || exit 10
    if touch %[1]s/escaped 2>/dev/null; then exit 11; fi
    touch "$TMPDIR/scratch" || exit 12
    case "$(cd "$TMPDIR" && pwd -P)" in %[2]s) exit 13;; esac
    if curl -s -m 3 %[3]s >/dev/null 2>&1; then exit 14; fi
    if touch %[2]s/steps-sandbox-escaped 2>/dev/null; then exit 15; fi

jobs:
- name: build
  plan:
  - task: probe
    inputs: []
`, outside, systemTmp, server.URL))

	err = cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("sandboxed task failed (exit 10: workspace not writable, 11: wrote outside, 12: no TMPDIR, 13: TMPDIR is the shared system one, 14: reached the network, 15: wrote the shared temp dir every workspace lives in): %v", err)
	}

	assertNoFile(t, filepath.Join(outside, "escaped"))
	assertNoFile(t, filepath.Join(systemTmp, "steps-sandbox-escaped"))

	if n := hits.Load(); n != 0 {
		t.Errorf("server saw %d requests from a network: none sandbox", n)
	}
}

// run_shell is the hand a model directs, so it is the command the sandbox exists for: the denial must come back to the model as tool output, not end the step.
func TestSandboxedAgentShellCannotWriteOutside(t *testing.T) {
	requireSandbox(t)

	dir := t.TempDir()
	outside := outsideDir(t)
	target := filepath.Join(outside, "agent-escaped")

	fake := newFakeLLM(t,
		callsTool("run_shell", map[string]any{"command": "touch " + target}),
		says("done"),
	)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  sandbox: true
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [run_shell]

jobs:
- name: publish
  plan:
  - agent: writer
    inputs: []
    messages: ["write it"]
`, fake.URL))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	err := cli.Run([]string{"run", path, "--job", "publish"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	assertNoFile(t, target)

	if !fake.request(2).toolResultContains("Operation not permitted") {
		t.Errorf("the model was not shown the denial: %v", fake.request(2).toolResults())
	}
}

// A resource type's commands are host commands too; check runs with no tree at all, so its only writable place is its private TMPDIR.
func TestSandboxedResourceTypeCannotWriteOutside(t *testing.T) {
	requireSandbox(t)

	dir := t.TempDir()
	outside := outsideDir(t)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: probe
  sandbox: true
  config:
    check: |
      touch %[1]s/from-check 2>/dev/null
      touch "$TMPDIR/scratch" || exit 1
      printf '[{"n": "1"}]'
    in: |
      touch %[1]s/from-in 2>/dev/null
      echo {{ .version.n | shellquote }} > n.txt

resources:
- name: ticks
  type: probe
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
  - task: show
    inputs: [ticks]
    run: cat ticks/n.txt
`, outside))

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, name := range []string{"from-check", "from-in"} {
		assertNoFile(t, filepath.Join(outside, name))
	}
}

func TestSandboxRefusesWhatItCannotFence(t *testing.T) {
	for _, tc := range []struct{ name, entry, want string }{
		{"image", "  image: alpine:3\n", "sandbox"},
		{"bridge network", "  network: bridge\n", "network"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, `
tasks:
- name: probe
  sandbox: true
`+tc.entry+`  run: "true"

jobs:
- name: build
  plan:
  - task: probe
    inputs: []
`)

			err := cli.Run([]string{"validate", "--syntax-only", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// A when: guard runs in the step's own runtime, so it is fenced too: a guard that could write outside would be a hole beside every sandboxed step. Each touch failing skips its step, which is why none of them needs a working endpoint or resource.
func TestSandboxedGuardsCannotWriteOutside(t *testing.T) {
	requireSandbox(t)

	dir := t.TempDir()
	outside := outsideDir(t)

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: sink
  sandbox: true
  config:
    check: printf '[]'
    out: |
      printf '{"n": "1"}'

resources:
- name: drop
  type: sink
  source: {}

tasks:
- name: work
  sandbox: true
  run: "true"

agents:
- name: helper
  sandbox: true
  source: {endpoint: http://127.0.0.1:1/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [run_shell]

jobs:
- name: build
  plan:
  - task: work
    inputs: []
    when: touch %[1]s/task-guard
  - agent: helper
    inputs: []
    messages: [x]
    when: touch %[1]s/agent-guard
  - put: drop
    inputs: []
    when: touch %[1]s/put-guard
`, outside))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	err := cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	for _, name := range []string{"task-guard", "agent-guard", "put-guard"} {
		assertNoFile(t, filepath.Join(outside, name))
	}
}

// The two other places an agent gets a shell: a task's fix: agent runs under the TASK's fence, and a delegated child under its own agents: entry's.
func TestSandboxReachesFixAndSubAgents(t *testing.T) {
	requireSandbox(t)

	escapes := func(target string) func(capturedRequest) turn {
		return func(req capturedRequest) turn {
			if req.historyCalled("run_shell") {
				return says("done")
			}

			return callsTool("run_shell", map[string]any{"command": "touch " + target})
		}
	}

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	t.Run("fix", func(t *testing.T) {
		target := filepath.Join(outsideDir(t), "fix-escaped")
		fixer := newRoutedFakeLLM(t, escapes(target))

		path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source: {endpoint: %s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools: [run_shell]

tasks:
- name: check
  sandbox: true
  run: exit 1
  fix: fixer

jobs:
- name: build
  plan:
  - task: check
    inputs: []
`, fixer.URL))

		_ = cli.Run([]string{"run", path, "--job", "build"}) // the task stays broken; where the fixer could write is the point

		if fixer.requestCount() == 0 {
			t.Fatal("the fix agent never ran")
		}

		assertNoFile(t, target)
	})

	t.Run("sub-agent", func(t *testing.T) {
		target := filepath.Join(outsideDir(t), "child-escaped")
		lead := newRoutedFakeLLM(t, func(req capturedRequest) turn {
			if req.historyCalled("helper") {
				return says("done")
			}

			return callsTool("helper", map[string]any{"request": "touch it"})
		})
		child := newRoutedFakeLLM(t, escapes(target))

		path := writePipeline(t, t.TempDir(), fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: helper
  sandbox: true
  source: {endpoint: %[2]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  description: Runs a command.
  tools: [run_shell]
- name: lead
  source: {endpoint: %[1]s/v1/, model: test-model, api_key_env: STEPS_TEST_AGENT_API_KEY}
  tools:
  - agent: helper
    description: Run a command.

jobs:
- name: triage
  plan:
  - agent: lead
    inputs: []
    messages: [delegate it]
`, lead.URL, child.URL))

		mustRun(t, "run", path, "--job", "triage")

		if child.requestCount() == 0 {
			t.Fatal("the child agent never ran")
		}

		assertNoFile(t, target)
	})
}
