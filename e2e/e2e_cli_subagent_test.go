package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

// TestE2ECLIAgentDelegatesToASubAgentOverTheBridge crosses the seam a CLI
// agent's delegation lives on: the parent is a subprocess, the child is a
// hosted conversation in THIS process, and the only thing between them is
// the bridge. A sub-agent tool is a toolImpl like any custom tool, so the
// bridge serves it without knowing what is behind it — this proves that by
// having the fake claude call the child and carry the child's own answer
// back out as its result.
//
// The child is routed to a fake provider of its own, so "the tool returned
// text" cannot be satisfied by anything but the child conversation actually
// running: the captured bridge response must carry what the PROVIDER said.
func TestE2ECLIAgentDelegatesToASubAgentOverTheBridge(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "summarizer.json")

	// The child's whole conversation: one reply, no tools.
	fake := newFakeLLM(t, says("the outage lasted four minutes"))

	claude := writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__summarizer") + "'",
		captureBridgeScript(captured, "summarizer", `{"request":"condense the notes"}`),
		"echo '" + cliToolUseEvent("t1", "mcp__steps__summarizer", `{"request":"condense the notes"}`) + "'",
		// The result is what the child said, read back off the bridge — a
		// fake that echoed a constant here would pass with the child skipped.
		// One match only: the bridge answers as SSE, so a line-wise sed would
		// fold "event: message" and its newline into the JSON being built.
		`said=$(grep -o 'lasted [a-z]* minutes' ` + fmt.Sprintf("%q", captured) + ` | head -1)`,
		`printf '{"type":"result","subtype":"success","result":"the helper says it %s","num_turns":1,"is_error":false,"usage":{"input_tokens":100,"output_tokens":20}}\n' "$said"`,
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: digest
  plan:
  - agent: lead
    inputs: []
    messages:
      - Have your summarizer condense the notes, then report.
    assert:
      tool_calls:
      - name: summarizer           # de-namespaced, as a hosted parent's trajectory spells it
      stdout: it lasted four minutes
  assert:
    execution: [lead]              # the child records nothing of its own
    outcome: succeeded
`, fake.URL+"/v1/"))

	mustRun(t, "test", path)

	if got := fake.requestCount(); got != 1 {
		t.Errorf("provider requests = %d, want 1: the child conversation is what the bridged call runs", got)
	}

	if got := readFileString(t, captured); !strings.Contains(got, "the outage lasted four minutes") {
		t.Errorf("the bridge answered the sub-agent call with something other than the child's reply:\n%s", got)
	}

	// The child is a capability grant like any other, so it is named on
	// --allowedTools under the bridge's namespace. Attestation compares that
	// list against the init event, so a grant missing here would kill the
	// step rather than silently withhold the delegation.
	if argv := claude.argv(t, 1); !strings.Contains(argv, "mcp__steps__summarizer") {
		t.Errorf("argv does not allow the sub-agent tool:\n%s", argv)
	}
}

// TestE2ECLIAgentSubAgentSpendReachesTheJob crosses the accounting half of
// the same seam. The hosted path publishes a conversation's accumulator to
// its tools (conversation.go, conv.env.usage) so a sub-agent call can size
// the child against what the parent has left and charge the child's spend
// back up the chain. The CLI runner attaches an accumulator of its own but
// used not to publish it, which left a delegating CLI step's child on its own
// declared budget with its tokens reaching NEITHER the parent nor the run's
// usage report — a delegation that spent real money and recorded none of it.
//
// The child's tokens are what this asserts, because they are the half that
// disappeared: the CLI parent itself reports no tokens at all (it meters in
// dollars), so the run's only token figure IS the delegation's.
func TestE2ECLIAgentSubAgentSpendReachesTheJob(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()

	fake := newFakeLLM(t, says("condensed").spending(4321))

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__summarizer") + "'",
		callBridgeScript("summarizer", `{"request":"condense the notes"}`),
		"echo '" + cliResultEvent("delegated", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: review
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate the summary.
`, fake.URL+"/v1/"))

	mustRun(t, path)

	if got := fake.requestCount(); got != 1 {
		t.Fatalf("provider requests = %d, want 1", got)
	}

	usage := agentUsageFor(t, path)

	// 4441 = the 120 the fake CLI reported for itself plus the 4321 the
	// delegation spent. Prompt/Completion stay the CLI's own figures: a
	// delegation's spend folds into the step's total, which is what a job
	// budget: counts and what `steps runs cost` prices.
	if usage.Total != 4441 {
		t.Errorf("recorded tokens = %d (prompt %d, completion %d), want 4441 — 120 the cli reported plus the 4321 its delegation spent",
			usage.Total, usage.Prompt, usage.Completion)
	}
}

// TestE2ECLIAgentConcurrentSubAgentCalls is the reentrancy half. A hosted
// conversation executes its tool calls one at a time, so a sub-agent tool had
// never been entered twice at once; a CLI child's calls arrive on the bridge's
// own HTTP goroutines, where they can. Two delegations in flight share one
// preparedSubAgent, one tool registry and one parent accumulator, so this runs
// under -race (as the whole suite does) and checks the accounting survives
// both of them.
func TestE2ECLIAgentConcurrentSubAgentCalls(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()

	// Repeating rather than scripted: the subject is that both delegations
	// ran and both were charged, not which one answered first.
	fake := newRepeatingFakeLLM(t, says("condensed").spending(1000))

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__summarizer") + "'",
		"(" + callBridgeScript("summarizer", `{"request":"first"}`) + ") &",
		"(" + callBridgeScript("summarizer", `{"request":"second"}`) + ") &",
		"wait",
		"echo '" + cliResultEvent("delegated twice", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: review
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate twice, at once.
`, fake.URL+"/v1/"))

	mustRun(t, path)

	if got := fake.requestCount(); got != 2 {
		t.Errorf("provider requests = %d, want 2: both concurrent delegations should have run", got)
	}

	// 2120 = the cli's own 120 plus 1000 from each delegation. A lost update
	// on the shared accumulator shows up here as 1120.
	if usage := agentUsageFor(t, path); usage.Total != 2120 {
		t.Errorf("recorded tokens = %d, want 2120: one of the two concurrent delegations was not charged", usage.Total)
	}
}

// TestE2ECLIAgentSubAgentIsPreflighted pins that the child of a CLI parent is
// probed before the parent starts. Preflight expands a job's agent list
// through every sub-agent grant it finds (withSubAgents), which is spelled by
// name and so covers a CLI parent without knowing it is one — the property
// worth a test precisely because nothing in that code mentions the CLI. A
// delegation is reached mid-run, which is the moment preflight exists to move
// earlier: a dead child should cost seconds, not a whole parent conversation.
func TestE2ECLIAgentSubAgentIsPreflighted(t *testing.T) {
	dir := t.TempDir()

	// Nothing listens here: a child that cannot be reached at all.
	claude := writeFakeClaude(t, "echo '"+cliResultEvent("never asked", 1)+"'")

	path := writePipeline(t, dir, `
agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: http://127.0.0.1:1/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: review
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate the summary.
`)

	err := cli.Run([]string{path})
	if err == nil {
		t.Fatal("run succeeded, want a preflight failure naming the unreachable sub-agent")
	}

	if !strings.Contains(err.Error(), "preflight") || !strings.Contains(err.Error(), "summarizer") {
		t.Errorf("error = %q, want it to name preflight and the sub-agent", err)
	}

	if got := claude.invocations(t); got != 0 {
		t.Errorf("the fake claude ran %d times, want 0: preflight failing means nothing ran", got)
	}
}

// TestE2ECLIAgentSubAgentRunsUnderTheJobBudget crosses the other half of the
// accounting seam: whose ledger the child conversation itself is bound by.
// The bridge serves its tools on the HTTP server's own goroutines, which
// net/http roots at context.Background() unless told otherwise — so the
// child's attachUsage found no job accumulator and a `budget: tokens:` the
// job declared neither counted the delegation nor stopped it. The child here
// spends fifty times the job's whole allowance and must be refused saying so,
// which only the job's own accumulator can produce.
func TestE2ECLIAgentSubAgentRunsUnderTheJobBudget(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()
	captured := filepath.Join(t.TempDir(), "summarizer.json")

	fake := newRepeatingFakeLLM(t, says("condensed").spending(50000))

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__summarizer") + "'",
		captureBridgeScript(captured, "summarizer", `{"request":"condense the notes"}`),
		"echo '" + cliResultEvent("delegated", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: review
  budget:
    tokens: 1000
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate the summary.
`, fake.URL+"/v1/"))

	mustRun(t, path)

	if got := readFileString(t, captured); !strings.Contains(got, "job budget exceeded") {
		t.Errorf("the delegation was not bound by the job's token ceiling; bridge answered:\n%s", got)
	}
}

// TestE2ECLIAgentSubAgentIsRecorded pins that a CLI parent's delegation is
// visible afterwards. runAgentConversation publishes its recorder to the
// tools (conv.env.transcript) and the CLI path does not run it, so the one
// tool that reads it — a sub-agent, which nests the child's whole transcript
// into the parent's and publishes the child's turns one level deeper — wrote
// into a nil recorder: a delegation that happened, spent money, and left no
// trace in the step's transcript or the live view.
func TestE2ECLIAgentSubAgentIsRecorded(t *testing.T) {
	requireCurl(t)

	dir := t.TempDir()

	fake := newFakeLLM(t, says("the outage lasted four minutes"))

	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliInitEvent("mcp__steps__summarizer") + "'",
		callBridgeScript("summarizer", `{"request":"condense the notes"}`),
		"echo '" + cliResultEvent("delegated", 1) + "'",
	}, "\n"))

	path := writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: summarizer
  system: "You summarize."
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

- name: lead
  source:
    model: "@claude/sonnet"
  tools:
  - agent: summarizer
    description: Condense a file; pass the path in request.

jobs:
- name: review
  plan:
  - agent: lead
    inputs: []
    messages:
      - Delegate the summary.
`, fake.URL+"/v1/"))

	mustRun(t, path)

	rows := agentEventsFor(t, path, "lead")

	delegation := findEvent(rows, "agent_subagent", "summarizer")
	if delegation == nil {
		t.Fatalf("no agent_subagent event recorded for the delegation; got %d agent events", len(rows))
	}

	if delegation.Text != "condense the notes" {
		t.Errorf("delegation event text = %q, want the request the parent authored", delegation.Text)
	}

	// The child's own turn, published one level deeper — what makes a
	// delegation that takes a minute visible while it runs.
	var childText *store.RunEventRow

	for i, row := range rows {
		if row.Type == "agent_text" && row.Status == "depth:1" {
			childText = &rows[i]

			break
		}
	}

	if childText == nil {
		t.Fatal("the child conversation published no events of its own")
	}

	if !strings.Contains(childText.Text, "the outage lasted four minutes") {
		t.Errorf("child event text = %q, want the child's reply", childText.Text)
	}
}
