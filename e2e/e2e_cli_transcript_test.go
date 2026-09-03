package e2e

// What a CLI agent leaves behind about HOW it worked.
//
// A CLI-backed agent runs its own tool loop in a subprocess, so it never
// enters internal/agent's conversation loop — the one that feeds the
// transcript recorder. The consequence, before this: a step that ran for six
// minutes published a start event and a finish event and nothing in between,
// and stored no transcript at all. The trajectory reached nodes.result and
// the model's own text reached nowhere.
//
// These assert against the STORE rather than the page, because the store is
// what both the live view and the replayed one read (see internal/web's
// CLAUDE.md). A row here is a turn on both.
//
// Not parallel: writeFakeClaude edits PATH for the process.

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestE2ECLIAgentPublishesItsConversation is the feature: the turns a CLI
// agent took are on the event bus as it takes them, under the step that took
// them.
func TestE2ECLIAgentPublishesItsConversation(t *testing.T) {
	dir := t.TempDir()

	writeFakeClaude(t, strings.Join([]string{
		`echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the diff first."}]}}'`,
		"echo '" + cliToolUseEvent("t1", "Read", `{"file_path":"main.go"}`) + "'",
		`echo '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"package main"}]}}'`,
		callBridgeScript("verdict", `{"choice":"approve"}`),
		"echo '" + cliResultEvent("Looks fine.", 2) + "'",
	}, "\n"))

	requireCurl(t)

	path := cliPipeline(t, dir)
	mustRun(t, "run", path, "--job", "review")

	rows := agentEventsFor(t, path, "reviewer")

	// The model's commentary, which recordCLIToolCalls dropped on the floor —
	// it walks the content blocks and keeps only tool_use.
	if text := findEvent(rows, events.TypeAgentText, ""); text == nil {
		t.Error("the model's text never reached the event bus")
	} else if !strings.Contains(text.Text, "Reading the diff first") {
		t.Errorf("agent_text carried %q", text.Text)
	}

	// A native CLI tool: one the parent never runs and only ever hears about
	// through the stream.
	if call := findEvent(rows, events.TypeAgentCall, "Read"); call == nil {
		t.Error("the cli's own tool call never reached the event bus")
	} else if !strings.Contains(call.Detail, "main.go") {
		t.Errorf("agent_call carried no arguments: %q", call.Detail)
	}

	if result := findEvent(rows, events.TypeAgentResult, "Read"); result == nil {
		t.Error("the tool result never reached the event bus")
	} else if !strings.Contains(result.Detail, "package main") {
		t.Errorf("agent_result carried %q", result.Detail)
	}
}

// TestE2ECLIAgentRecordsABridgedCallExactlyOnce is the double-recording trap.
// A bridged tool is executed by the PARENT, and also reported by the child's
// own stream — recording both views would show every verdict, every custom
// tool, and every MCP call twice.
func TestE2ECLIAgentRecordsABridgedCallExactlyOnce(t *testing.T) {
	dir := t.TempDir()

	// count_lines is a CUSTOM tool the pipeline grants, so it can only reach
	// the parent through the bridge — the shape that is both executed here and
	// reported by the child, and therefore the one at risk of double-counting.
	writeFakeClaude(t, strings.Join([]string{
		"echo '" + cliToolUseEvent("c1", "mcp__steps__count_lines", `{"path":"main.go"}`) + "'",
		callBridgeScript("count_lines", `{"path":"main.go"}`),
		callBridgeScript("verdict", `{"choice":"approve"}`),
		"echo '" + cliResultEvent("Approved.", 1) + "'",
	}, "\n"))

	requireCurl(t)

	path := cliPipeline(t, dir)
	mustRun(t, "run", path, "--job", "review")

	rows := agentEventsFor(t, path, "reviewer")

	calls := 0

	for _, row := range rows {
		if row.Type == events.TypeAgentCall && strings.Contains(row.Name, "count_lines") {
			calls++
		}
	}

	if calls != 1 {
		t.Errorf("the bridged count_lines call was recorded %d times, want 1", calls)
	}
}

// TestE2ECLIAgentStoresItsTranscript is the other half of the same record: a
// run read back an hour later has to say what a run watched live said. The
// node page's Conversation section reads this row.
func TestE2ECLIAgentStoresItsTranscript(t *testing.T) {
	dir := t.TempDir()

	writeFakeClaude(t, strings.Join([]string{
		`echo '{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it."}]}}'`,
		callBridgeScript("verdict", `{"choice":"approve"}`),
		"echo '" + cliResultEvent("Done.", 1) + "'",
	}, "\n"))

	requireCurl(t)

	path := cliPipeline(t, dir)
	mustRun(t, "run", path, "--job", "review")

	st := openStoreFor(t, path)

	nodes, err := st.ListNodes(t.Context(), "", 50)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}

	stored := ""

	for _, node := range nodes {
		if node.Kind != "agent" {
			continue
		}

		transcript, ok, readErr := st.NodeTranscript(t.Context(), node.Hash)
		if readErr != nil {
			t.Fatalf("NodeTranscript: %v", readErr)
		}

		if ok {
			stored = transcript
		}
	}

	if stored == "" {
		t.Fatal("a cli agent step stored no transcript")
	}

	if !strings.Contains(stored, "Working on it") {
		t.Errorf("the stored transcript does not carry the model's text: %s", stored)
	}
}

// TestE2ECLIAgentRecordsWhatItSpent covers the spend panel's own blind spot: a
// CLI reports cache hits, a finish reason and a dollar figure, and all three
// were parsed and thrown away — so the page showed a million cache-read tokens
// as "0% cached" and priced a billable run at nothing.
func TestE2ECLIAgentRecordsWhatItSpent(t *testing.T) {
	dir := t.TempDir()

	writeFakeClaude(t, strings.Join([]string{
		callBridgeScript("verdict", `{"choice":"approve"}`),
		`echo '{"type":"result","subtype":"success","result":"Done.","num_turns":1,` +
			`"is_error":false,"total_cost_usd":0.0425,` +
			`"usage":{"input_tokens":10,"output_tokens":20,` +
			`"cache_creation_input_tokens":300,"cache_read_input_tokens":4000}}'`,
	}, "\n"))

	requireCurl(t)

	path := cliPipeline(t, dir)
	mustRun(t, "run", path, "--job", "review")

	usage := agentUsageFor(t, path)

	// 10 + 300 + 4000 input, of which 4300 were served from cache.
	if usage.Cached != 4300 {
		t.Errorf("cached tokens = %d, want 4300", usage.Cached)
	}

	if usage.Prompt != 4310 {
		t.Errorf("prompt tokens = %d, want 4310", usage.Prompt)
	}

	if usage.CostUSD == nil || *usage.CostUSD == 0 {
		t.Error("the cli reported total_cost_usd and it was not recorded")
	}

	if usage.FinishReason == "" {
		t.Error("the cli reported how it finished and it was not recorded")
	}
}

// agentEventsFor reads the agent conversation events recorded for one step.
func agentEventsFor(t *testing.T, path, step string) []store.RunEventRow {
	t.Helper()

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "review", 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 5000)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	out := make([]store.RunEventRow, 0, len(rows))

	for _, row := range rows {
		if row.StepName == step && strings.HasPrefix(row.Type, "agent_") {
			out = append(out, row)
		}
	}

	if len(out) == 0 {
		t.Fatalf("no agent events recorded for step %q", step)
	}

	return out
}

// agentUsageFor reads the one agent_usage row a single-agent run records.
func agentUsageFor(t *testing.T, path string) store.AgentUsage {
	t.Helper()

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "review", 10)
	if err != nil || len(runs) == 0 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	usage, err := st.RunUsage(t.Context(), runs[0].ID)
	if err != nil {
		t.Fatalf("RunUsage: %v", err)
	}

	if len(usage) != 1 {
		t.Fatalf("got %d usage rows, want 1: %+v", len(usage), usage)
	}

	return usage[0]
}

// openStoreFor opens the state database the pipeline at path wrote, through
// the same statePath() every other end-to-end test derives it with.
func openStoreFor(t *testing.T, path string) *store.Store {
	t.Helper()

	st, err := store.OpenStore(cli.StatePath(path, ""), cli.PipelineName(path))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

// findEvent returns the first event of a type, optionally narrowed by the tool
// name it names.
func findEvent(rows []store.RunEventRow, eventType, name string) *store.RunEventRow {
	for i, row := range rows {
		if row.Type != eventType {
			continue
		}

		if name == "" || row.Name == name {
			return &rows[i]
		}
	}

	return nil
}

// requireCurl skips a test whose fake CLI has to call back into the bridge.
func requireCurl(t *testing.T) {
	t.Helper()

	_, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is needed to call the bridge from a shell-script cli")
	}
}
