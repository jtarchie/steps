package main

// End-to-end coverage for the run context store (context: write / set_context).
//
// Lives in the root package for the reason every e2e test here does: only
// main's run() spans CLI → config → merkle → agent conversation → store, and
// source.endpoint: is the only injection point for a scripted model.

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// contextPipeline writes a two-agent pipeline where the first step is granted
// context: write and the second is a plain agent step. No resources: the
// subject is the tool and the store, and a get would only add moving parts.
func contextPipeline(t *testing.T, dir, endpoint string) string {
	t.Helper()

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: investigator
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  system: You investigate.
  tools:
  - builtin: read_file

jobs:
- name: triage
  plan:
  - agent: investigator
    prompt: Investigate the failure.
    context: write
`, endpoint)

	return writePipeline(t, dir, yaml)
}

// TestEndToEndContextWrite is the whole feature on one pass: the synthesized
// tool reaches the wire only because the step declared context: write, a call
// to it lands as a row in run_context attributed to the writing step, and a
// key the model has no business writing is refused as data rather than
// failing the step.
func TestEndToEndContextWrite(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t,
		// A reserved key: refused at the tool boundary, fed back as an
		// error the model can react to. The step must still succeed.
		callsTool("set_context", map[string]any{
			"key":   "internal.run_id",
			"value": "hijacked",
		}),
		callsTool("set_context", map[string]any{
			"key":   "failure_cause",
			"value": "flaky DNS in the e2e suite",
		}),
		says("Investigated."),
	)
	path := contextPipeline(t, dir, fake.URL)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	// ── wire layer ────────────────────────────────────────────────────────
	// set_context is offered alongside the step's own grant. Its presence is
	// the proof that the config declaration compiled into a tool set; the
	// read_file beside it proves the declared grant was not replaced.
	wantTools := []string{"read_file", "set_context"}
	if got := fake.request(1).toolNames(); !slices.Equal(got, wantTools) {
		t.Errorf("request 1 offered tools = %v, want %v", got, wantTools)
	}

	// ── tool-boundary layer ───────────────────────────────────────────────
	// The reserved-key call came back as an error, not a step failure: the
	// model gets a turn to correct itself, which is the contract every tool
	// in this codebase honors.
	refusal := fake.request(2).toolResults()
	if len(refusal) != 1 {
		t.Fatalf("request 2 carried %d tool results, want 1; got %v", len(refusal), refusal)
	}

	if !strings.Contains(refusal[0], "reserved") {
		t.Errorf("reserved-key call result = %q, want it to name the reserved prefix", refusal[0])
	}

	// ── store layer ───────────────────────────────────────────────────────
	entries := storeRunContext(t, path)

	// Exactly one row: the refused write stored nothing at all.
	if len(entries) != 1 {
		t.Fatalf("run_context rows = %+v, want exactly 1 (the refused write must store nothing)", entries)
	}

	if entries[0].Key != "failure_cause" || entries[0].Value != "flaky DNS in the e2e suite" {
		t.Errorf("stored entry = %+v, want failure_cause with the model's value", entries[0])
	}

	// Attribution is the reason written_by exists: the row answers "who
	// recorded this" without replaying the transcript.
	if entries[0].WrittenBy != "investigator" {
		t.Errorf("written_by = %q, want the writing step's name", entries[0].WrittenBy)
	}
}

// TestEndToEndContextNotOfferedWithoutDeclaration pins the opt-in: the same
// pipeline without context: write must not put set_context on the wire.
// Without this, a passing write test proves only that the tool exists, not
// that declaring it is what summons it.
func TestEndToEndContextNotOfferedWithoutDeclaration(t *testing.T) {
	dir := t.TempDir()
	fake := newFakeLLM(t, says("Investigated."))

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: investigator
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: read_file

jobs:
- name: triage
  plan:
  - agent: investigator
    prompt: Investigate the failure.
`, fake.URL)
	path := writePipeline(t, dir, yaml)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, path)

	if got := fake.request(1).toolNames(); slices.Contains(got, "set_context") {
		t.Errorf("offered tools = %v; set_context must not appear without context: write", got)
	}

	if entries := storeRunContext(t, path); len(entries) != 0 {
		t.Errorf("run_context rows = %+v, want none", entries)
	}
}

// contextRow is one run_context row, read back for assertions.
type contextRow struct {
	Key       string
	Value     string
	WrittenBy string
}

func storeRunContext(t *testing.T, pipelinePath string) []contextRow {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(), `SELECT key, value, written_by FROM run_context ORDER BY key`)
	if err != nil {
		t.Fatalf("query run_context: %v", err)
	}
	defer func() { _ = rows.Close() }()

	return scanContextRows(t, rows)
}

func scanContextRows(t *testing.T, rows *sql.Rows) []contextRow {
	t.Helper()

	var entries []contextRow

	for rows.Next() {
		var entry contextRow

		err := rows.Scan(&entry.Key, &entry.Value, &entry.WrittenBy)
		if err != nil {
			t.Fatalf("scan run_context: %v", err)
		}

		entries = append(entries, entry)
	}

	err := rows.Err()
	if err != nil {
		t.Fatalf("read run_context: %v", err)
	}

	return entries
}
