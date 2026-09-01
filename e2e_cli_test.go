package main

import (
	"fmt"
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
  reasoning_effort: high
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
    messages:
      - Review the diff.
    verdicts:
      - approve: celebrate
      - reject: escalate
      - failure: escalate
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
		// A pipeline step must not inherit the operator's personal ~/.claude,
		// nor the repo's .claude/ scope without a settings: declaration —
		// empty means the child loads no configuration scopes at all.
		"--setting-sources||",
		// The built-in surface is an allow-list of exactly what was granted.
		"--tools|Bash,Read|",
		// reasoning_effort: reaches the CLI as its own --effort dial rather
		// than being refused: the CLI grew one, and a step that asks for
		// deeper reasoning should get it on either runner.
		"--effort|high|",
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

	// Nothing ungranted appears anywhere on the command line. Under --tools
	// there is no deny list to name them on: they are withheld by omission,
	// which is what makes a built-in this build has never heard of safe.
	for _, unwanted := range []string{"Write", "Edit", "Grep", "Task", "WebFetch", "WebSearch"} {
		if strings.Contains(argv, unwanted) {
			t.Errorf("ungranted %q appears in argv:\n%s", unwanted, argv)
		}
	}

	if prompt := cli.prompt(t, 1); !strings.Contains(prompt, "Review the diff.") {
		t.Errorf("the prompt did not reach the cli's stdin:\n%s", prompt)
	}
}

func TestE2ECLIAgentVerdictRoutesTheJob(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()

	// The fake CLI calls the verdict tool on the parent's bridge — the same
	// round trip the real CLI makes — then reports itself finished.
	writeFakeClaude(t,
		callBridgeScript("verdict", `{"choice":"approve","note":"ship it"}`)+
			"echo '"+cliToolUseEvent("t1", "mcp__steps__verdict", `{"choice":"approve"}`)+"'\n"+
			"echo '"+cliResultEvent("approved", 1)+"'")

	path := cliPipeline(t, dir)

	err := run([]string{path})
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
	requireCurl(t)

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
		"messages: [Review the diff.]",
		"messages: [Review the diff.]\n    attempts: 2",
		1)
	path := writePipeline(t, dir, yaml)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := cli.invocations(t); got != 2 {
		t.Errorf("the cli ran %d times, want 2 (one failed attempt, one retry)", got)
	}

	if got := readFileString(t, filepath.Join(dir, "approved.log")); !strings.Contains(got, "approved") {
		t.Error("the retry's verdict did not route")
	}

	// The retry REJOINED the first attempt's conversation rather than starting
	// the task over. That is the whole point: a restart leaves the agent
	// holding its own half-finished edits with no memory of making them, which
	// is why the hosted path stopped doing it (see requests.go).
	opening, retried := cli.argv(t, 1), cli.argv(t, 2)

	session := fieldAfter(t, opening, "--session-id")
	if session == "" {
		t.Fatalf("the opening invocation named no session:\n%s", opening)
	}

	if got := fieldAfter(t, retried, "--resume"); got != session {
		t.Errorf("retry resumed %q, want the opening session %q", got, session)
	}

	if strings.Contains(retried, "--session-id|") {
		t.Errorf("the retry started a new session instead of resuming:\n%s", retried)
	}

	// It was told to continue, not handed the task again.
	if prompt := cli.prompt(t, 2); !strings.Contains(prompt, "do not start the task over") {
		t.Errorf("the retry was not told to continue:\n%s", prompt)
	}

	if prompt := cli.prompt(t, 2); strings.Contains(prompt, "Review the diff.") {
		t.Errorf("the retry was re-sent the original task:\n%s", prompt)
	}
}

// fieldAfter returns the "|"-separated argv field following flag, or "".
func fieldAfter(t *testing.T, argv, flag string) string {
	t.Helper()

	fields := strings.Split(argv, "|")
	for i, field := range fields {
		if field == flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}

	return ""
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
		"messages: [Review the diff.]",
		"messages: [Review the diff.]\n    attempts: 3",
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

// TestE2ECLIAgentsRunConcurrently covers matrix fan-out over a CLI agent,
// which is where the per-attempt bridge stops being an implementation detail:
// every cell spawns its own subprocess AND its own MCP server, so a cell
// reading another cell.s captured verdict would route the wrong branch.
// Nothing about that is exercised by a sequential test.
func TestE2ECLIAgentsRunConcurrently(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()

	// Each cell votes for the branch named after itself, so a crossed wire
	// between two bridges shows up as the wrong log file being written rather
	// than as a silent pass.
	cli := writeFakeClaude(t,
		callBridgeScript("verdict", `{"choice":"approve"}`)+
			"echo '"+cliResultEvent("done", 1)+"'")

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

workspace:
  strategy: copy

agents:
- name: reviewer
  source:
    model: "@claude/sonnet"
  tools: [read_file]

jobs:
- name: review
  plan:
  - across:
    - var: cell
      values: [a, b, c]
    max_in_flight: 3
    agent: reviewer
    inputs: []
    outputs: []
    messages:
      - Review cell {{ .vars.cell }}.
    verdicts:
      - approve: next
  - task: record
    inputs: []
    run: echo joined >> %s
`, filepath.Join(dir, "joined.log"))

	path := writePipeline(t, dir, yaml)

	err := run([]string{path})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := cli.invocations(t); got != 3 {
		t.Fatalf("the cli ran %d times, want one per cell (3)", got)
	}

	// Every cell got its OWN bridge. Sharing one would mean one cell.s verdict
	// capture could satisfy another cell.s obligation, which is precisely the
	// failure a sequential test cannot see.
	configs := map[string]bool{}

	for _, argv := range cli.records(t, "argv") {
		for _, field := range strings.Split(argv, "|") {
			if strings.Contains(field, "steps-cli-mcp") {
				configs[field] = true
			}
		}
	}

	if len(configs) != 3 {
		t.Errorf("cells shared %d mcp config(s), want 3 distinct bridges: %v", len(configs), configs)
	}

	// All three cells routed on their own verdict, so the join was reached.
	if got := readFileString(t, filepath.Join(dir, "joined.log")); !strings.Contains(got, "joined") {
		t.Error("the step after the matrix did not run; a cell did not route")
	}
}
