package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// End-to-end coverage of CLI-backed agent steps (`source.model: "@claude/…"`).
//
// These are the only tests that prove the whole delegation works as one pass:
// a pipeline YAML, a fake `claude` on PATH, and assertions at every layer the
// step crosses — the argument vector the child was given (which IS the
// permission boundary), the prompt it received on stdin, the tool call it made
// back into the parent's MCP bridge, the verdict that routed the job, and the
// rows the store ended up with.
//
// They cannot be package-level parallel: installing a fake binary means
// editing PATH for the process.

// cliPipeline is the fixture: one CLI agent with a verdict route, granted two
// built-ins (which become the CLI's native tools) and one custom tool (which
// can only reach it through the bridge).
func cliPipeline(t *testing.T, dir string) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source:
    model: "@claude/sonnet"
  system: You review code.
  max_turns: 12
  tools:
  - read_file
  - run_shell
  - name: count_lines
    description: Count the lines in a file.
    run: wc -l < {{ .args.path }}

jobs:
- name: review
  plan:
  - agent: reviewer
    inputs: []
    prompt: Review the diff.
    verdicts: [approve, reject]
    to:
      approve: celebrate
      reject: escalate
      failure: escalate
  - task: escalate
    inputs: []
    run: |
      echo escalated >> %[2]s
      exit 1
  - task: celebrate
    inputs: []
    run: echo approved >> %[1]s
`, filepath.Join(dir, "approved.log"), filepath.Join(dir, "escalated.log"))

	return writePipeline(t, dir, yaml)
}

func TestE2ECLIAgentInvocation(t *testing.T) {
	dir := t.TempDir()
	cli := writeFakeClaude(t, "echo '"+cliResultEvent("looks fine", 2)+"'")
	path := cliPipeline(t, dir)

	// No verdict is emitted, so the step fails its declared-verdicts
	// obligation — which is itself the assertion in TestE2ECLIAgentVerdict.
	// Here the run's outcome is beside the point: what this pins is what the
	// child was ASKED to do.
	_ = run([]string{path})

	argv := cli.argv(t, 1)

	// Arguments are recorded "|"-separated, so these match whole values
	// rather than substrings of a flattened command line.
	for _, want := range []string{
		"--print|",
		"--output-format|stream-json|",
		"--model|sonnet|",
		"--max-turns|12|",
		"--append-system-prompt|You review code.",
		"--strict-mcp-config|",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv is missing %q:\n%s", want, argv)
		}
	}

	// The grant, translated: granted built-ins become the CLI's own native
	// tools, and the custom tool is reachable only through the bridge.
	for _, want := range []string{"Read", "Bash", "mcp__steps__count_lines", "mcp__steps__verdict"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv does not allow %q:\n%s", want, argv)
		}
	}

	// Ungranted built-ins must be denied outright, not merely left off the
	// allow-list: a CLI's read-only tools need no permission in -p mode.
	for _, want := range []string{"Write", "Edit", "Grep", "Task", "WebFetch"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv does not deny %q:\n%s", want, argv)
		}
	}

	if prompt := readFileString(t, cli.promptPath); !strings.Contains(prompt, "Review the diff.") {
		t.Errorf("the prompt did not reach the cli's stdin:\n%s", prompt)
	}
}

func TestE2ECLIAgentVerdictRoutesTheJob(t *testing.T) {
	_, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is needed to call the bridge from a shell-script cli")
	}

	dir := t.TempDir()

	// The fake CLI calls the verdict tool on the parent's bridge — the same
	// round trip the real CLI makes — then reports itself finished.
	writeFakeClaude(t,
		callBridgeScript("verdict", `{"choice":"approve","note":"ship it"}`)+
			"echo '"+cliToolUseEvent("t1", "mcp__steps__verdict", `{"choice":"approve"}`)+"'\n"+
			"echo '"+cliResultEvent("approved", 1)+"'")

	path := cliPipeline(t, dir)

	err = run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The verdict was captured in the parent's memory, over a channel the
	// child never had to be trusted to report honestly, and it routed.
	if got := readFileString(t, filepath.Join(dir, "approved.log")); !strings.Contains(got, "approved") {
		t.Error("the approve branch did not run")
	}

	assertNoFile(t, filepath.Join(dir, "escalated.log"))

	agentNode := findNode(t, storeNodes(t, path), "agent", "reviewer")
	if agentNode.Status != "succeeded" {
		t.Errorf("agent node status = %q (%s), want succeeded", agentNode.Status, agentNode.Error)
	}
}

// TestE2ECLIAgentWithoutVerdictFails pins the enforcement that replaces
// tool_choice across the process boundary: a step that declared verdicts: and
// got none is a failure, not a success with an empty verdict.
func TestE2ECLIAgentWithoutVerdictFails(t *testing.T) {
	dir := t.TempDir()
	writeFakeClaude(t, "echo '"+cliResultEvent("I have decided nothing.", 1)+"'")
	path := cliPipeline(t, dir)

	err := run([]string{path})
	if err == nil {
		t.Fatal("a step that never emitted its declared verdict reported success")
	}

	agentNode := findNode(t, storeNodes(t, path), "agent", "reviewer")
	if agentNode.Status != "failed" {
		t.Errorf("agent node status = %q, want failed", agentNode.Status)
	}

	if !strings.Contains(agentNode.Error, "verdict") {
		t.Errorf("recorded error = %q, want it to name the missing verdict", agentNode.Error)
	}

	// A missing verdict is a failure the pipeline can route on, so the
	// failure: branch ran rather than the job simply erroring out.
	if got := readFileString(t, filepath.Join(dir, "escalated.log")); !strings.Contains(got, "escalated") {
		t.Error("the failure branch did not run")
	}
}

// TestE2ECLIAgentRetriesInfrastructureFailure pins what attempts: means for a
// process that cannot be resumed: the whole invocation is re-run, but only
// when it failed to RUN. A CLI that ran and reported a bad outcome is not
// re-rolled (see TestE2ECLIAgentDoesNotRetryTaskFailure).
func TestE2ECLIAgentRetriesInfrastructureFailure(t *testing.T) {
	_, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is needed to call the bridge from a shell-script cli")
	}

	dir := t.TempDir()

	// First invocation dies without a result event; the second behaves.
	cli := writeFakeClaude(t, `
if [ ! -f "$0.attempted" ]; then
  touch "$0.attempted"
  echo '{"type":"assistant","message":{"content":[{"type":"text","text":"dying"}]}}'
  exit 3
fi
`+callBridgeScript("verdict", `{"choice":"approve"}`)+
		"echo '"+cliResultEvent("recovered", 1)+"'")

	yaml := strings.Replace(
		readFileString(t, cliPipeline(t, dir)),
		"prompt: Review the diff.",
		"prompt: Review the diff.\n    attempts: 2",
		1)
	path := writePipeline(t, dir, yaml)

	err = run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := cli.invocations(t); got != 2 {
		t.Errorf("the cli ran %d times, want 2 (one failed attempt, one retry)", got)
	}

	if got := readFileString(t, filepath.Join(dir, "approved.log")); !strings.Contains(got, "approved") {
		t.Error("the retry's verdict did not route")
	}
}

// TestE2ECLIAgentDoesNotRetryTaskFailure is the other half of attempts:. The
// CLI ran fine and concluded the task failed — an answer, not an outage.
// Retrying would pay twice for the same conclusion.
func TestE2ECLIAgentDoesNotRetryTaskFailure(t *testing.T) {
	dir := t.TempDir()

	// Exits 1 the way the real binary does when it reports a task failure:
	// the whole point of the finding this pins is that the exit status must
	// not turn a reported failure into a retryable infrastructure error.
	cli := writeFakeClaude(t,
		"echo '"+cliErrorResultEvent("error_max_turns", "Reached maximum number of turns (12)")+"'\nexit 1")

	yaml := strings.Replace(
		readFileString(t, cliPipeline(t, dir)),
		"prompt: Review the diff.",
		"prompt: Review the diff.\n    attempts: 3",
		1)
	path := writePipeline(t, dir, yaml)

	err := run([]string{path})
	if err == nil {
		t.Fatal("a cli that reported the task as failed produced a successful run")
	}

	if got := cli.invocations(t); got != 1 {
		t.Errorf("the cli ran %d times, want 1 — a reported task failure must not be retried", got)
	}

	// Reported failures are routable, so the failure: branch ran. An exit
	// status read as an infrastructure error would have errored the job
	// instead, and no to: key could have caught it.
	if got := readFileString(t, filepath.Join(dir, "escalated.log")); !strings.Contains(got, "escalated") {
		t.Error("the failure branch did not run; the reported failure was not classified as a step failure")
	}

	agentNode := findNode(t, storeNodes(t, path), "agent", "reviewer")
	if !strings.Contains(agentNode.Error, "Reached maximum number of turns") {
		t.Errorf("recorded error = %q, want the cli's own message rather than a bare subtype", agentNode.Error)
	}
}
