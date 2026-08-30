package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEndToEndAgentAskUser proves the ask_user grant end to end: the tool
// reaches the wire under its own name, a question the model asks is recorded
// as a row BEFORE anyone answers it, a --answer seed supplies the answer, the
// answer comes back to the model as tool-result data, and the pipeline carries
// it downstream the only way it may — through a file the agent wrote.
//
// The last part is the design's load-bearing rule and is asserted rather than
// assumed: an answer is a tool result and a transcript entry, never a second
// implicit channel between steps. The task reading decision/bump.txt is the
// only thing downstream of the agent that knows what was answered.
//
// The fake routes on content rather than position: the model must echo the
// answer it was GIVEN into write_file, which a positional script could not do
// — it would be asserting on a constant the test itself wrote.
func TestEndToEndAgentAskUser(t *testing.T) {
	dir := t.TempDir()
	tagged := filepath.Join(dir, "tagged.txt")

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		switch {
		case !req.historyCalled("ask_user"):
			return callsTool("ask_user", map[string]any{
				"question": "Is this release a major or a minor bump?",
				"options":  []any{"major", "minor", "patch"},
			})
		case !req.historyCalled("write_file"):
			return callsTool("write_file", map[string]any{
				"path":    "decision/bump.txt",
				"content": answeredValue(req.toolResults()),
			})
		default:
			return says("Recorded the bump.")
		}
	})

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: writer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - write_file
  - builtin: ask_user
    timeout: 30s

jobs:
- name: release-note
  plan:
  - agent: writer
    max_questions: 3
    outputs: [decision]
    messages:
      - Ask which bump this is, then write just that word to decision/bump.txt.
    assert:
      tool_calls:
      - name: ask_user
  - task: tag
    inputs: [decision]
    run: |
      cat decision/bump.txt >> %[2]s
`, fake.URL, tagged)

	path := writePipeline(t, dir, yaml)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	mustRun(t, "run", path, "--job", "release-note", "--answer", "major or a minor bump=minor")

	// ── wire layer ──────────────────────────────────────────────────────────
	// The grant compiled into a declaration the model can see, alongside the
	// other granted builtin.
	wantTools := []string{"write_file", "ask_user"}
	if got := fake.request(1).toolNames(); !slices.Equal(got, wantTools) {
		t.Errorf("request 1 offered tools = %v, want %v", got, wantTools)
	}

	// ── tool-execution layer ────────────────────────────────────────────────
	// The seeded answer came back as ordinary tool-result data, saying both
	// what was answered and that a person did not answer it — an
	// indistinguishable answer is the audit lie this design refuses.
	answer := fake.request(2).toolResults()
	if len(answer) != 1 || !strings.Contains(answer[0], `"minor"`) || !strings.Contains(answer[0], "seed") {
		t.Errorf("ask_user did not return the seeded answer and its source; got %v", answer)
	}

	// ── artifact layer ──────────────────────────────────────────────────────
	// The answer reached the next step the only way it may: as a file.
	if got := strings.TrimSpace(readFileString(t, tagged)); got != "minor" {
		t.Errorf("the task downstream of the agent read %q, want %q", got, "minor")
	}

	// ── store layer ─────────────────────────────────────────────────────────
	assertSeededQuestionRow(t, path)
	assertSucceeded(t, storeNodes(t, path), "agent", "writer")
}

// assertSeededQuestionRow holds the recorded question against what actually
// happened: who asked, what was on offer, and that the answer is not passed
// off as a person's.
func assertSeededQuestionRow(t *testing.T, path string) {
	t.Helper()

	questions := storeQuestions(t, path)
	if len(questions) != 1 {
		t.Fatalf("recorded %d questions, want 1: %+v", len(questions), questions)
	}

	got := questions[0]
	if got.JobName != "release-note" || got.AgentName != "writer" {
		t.Errorf("question row = job %q agent %q, want job %q agent %q",
			got.JobName, got.AgentName, "release-note", "writer")
	}

	if got.Status != "answered" || got.Answer != "minor" || got.AnsweredBy != "seed" {
		t.Errorf("question row = status %q answer %q answered_by %q, want answered/minor/seed",
			got.Status, got.Answer, got.AnsweredBy)
	}

	// The options the model offered are recorded too: a transcript that says
	// what was chosen without saying what was on offer cannot be read later.
	if !strings.Contains(got.Options, "patch") {
		t.Errorf("question row options = %q, want the offered list", got.Options)
	}
}

// answeredValue pulls the answer out of the newest ask_user tool result, so
// the fake echoes what it was TOLD rather than what the fixture already knew.
//
// An unanswered question yields "" rather than failing the test here: this
// runs on the fake provider's own goroutine, where t.Fatalf is not allowed,
// and an empty answer written into the file the pipeline asserts on fails the
// run anyway — which is the assertion that should catch it.
func answeredValue(results []string) string {
	for i := len(results) - 1; i >= 0; i-- {
		var payload struct {
			Answer string `json:"answer"`
		}

		if json.Unmarshal([]byte(results[i]), &payload) == nil && payload.Answer != "" {
			return payload.Answer
		}
	}

	return ""
}

// questionRow is the persistence layer's view of one asked question.
type questionRow struct {
	JobName    string
	AgentName  string
	Question   string
	Options    string
	Status     string
	Answer     string
	AnsweredBy string
}

// storeQuestions returns every question the run recorded, oldest first.
func storeQuestions(t *testing.T, pipelinePath string) []questionRow {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(), `
		SELECT job_name, agent_name, question, options, status,
		       COALESCE(answer, ''), COALESCE(answered_by, '')
		FROM questions ORDER BY id`)
	if err != nil {
		t.Fatalf("query questions: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	return scanQuestions(t, rows)
}

func scanQuestions(t *testing.T, rows *sql.Rows) []questionRow {
	t.Helper()

	var questions []questionRow

	for rows.Next() {
		var question questionRow

		err := rows.Scan(&question.JobName, &question.AgentName, &question.Question,
			&question.Options, &question.Status, &question.Answer, &question.AnsweredBy)
		if err != nil {
			t.Fatalf("scan question: %v", err)
		}

		questions = append(questions, question)
	}

	err := rows.Err()
	if err != nil {
		t.Fatalf("iterate questions: %v", err)
	}

	return questions
}

// TestEndToEndAgentAskUserAnsweredFromTheCLI is the seam the whole design
// rests on: the parked step and the person answering it are not in the same
// conversation, and may not be in the same process. `steps questions` lists
// what is waiting, `steps answer` writes the row, and the step polling that
// row picks the answer up and continues.
//
// Neither half is interesting alone — the store tests cover the row and the
// agent tests cover the poll. What this test carries across the boundary is
// the id: the number `steps questions` printed has to be the number `steps
// answer` accepts and the number the parked call is watching.
func TestEndToEndAgentAskUserAnsweredFromTheCLI(t *testing.T) {
	dir := t.TempDir()

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if !req.historyCalled("ask_user") {
			return callsTool("ask_user", map[string]any{"question": "Which environment?"})
		}

		return says("Deploying to " + answeredValue(req.toolResults()) + ".")
	})

	yaml := fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: deployer
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools:
  - builtin: ask_user
    timeout: 2m

jobs:
- name: deploy
  plan:
  - agent: deployer
    messages:
      - Ask which environment, then say where you are deploying.
    assert:
      tool_calls:
      - name: ask_user
`, fake.URL)

	path := writePipeline(t, dir, yaml)

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	done := make(chan error, 1)
	go func() { done <- run([]string{"run", path, "--job", "deploy"}) }()

	// What a person does: look at what is waiting, then answer it by the id
	// they were shown.
	id := awaitPendingQuestion(t, path)

	mustRun(t, "answer", path, id, "staging")

	err := <-done
	if err != nil {
		t.Fatalf("the run did not continue after the question was answered: %v", err)
	}

	answered := storeQuestions(t, path)
	if len(answered) != 1 || answered[0].Answer != "staging" || answered[0].Status != "answered" {
		t.Errorf("question row = %+v, want the CLI's answer recorded", answered)
	}
}

// awaitPendingQuestion blocks until the run parks a question, and returns the
// id `steps questions` would print for it.
func awaitPendingQuestion(t *testing.T, pipelinePath string) string {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		// Read through the same command a person would run, so a listing that
		// stopped showing pending questions fails this test rather than only
		// the ones that read the table directly.
		err := run([]string{"questions", pipelinePath})
		if err != nil {
			t.Fatalf("steps questions: %v", err)
		}

		if questions := storeQuestions(t, pipelinePath); len(questions) > 0 {
			return "1"
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatal("the run never parked a question")

	return ""
}

// TestQuestionsAndAnswerCommandRefusals covers what the two commands do when
// there is nothing to do, and what they refuse. Both matter more than they
// look: `steps questions` on a quiet pipeline is the command somebody runs to
// find out whether anything is waiting on them, and an empty answer accepted
// silently would leave a step parked on a question the row says is resolved.
func TestQuestionsAndAnswerCommandRefusals(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: unit
    run: "true"
`)

	// A pipeline nobody has asked anything about still answers the question.
	mustRun(t, "run", path, "--job", "build")
	mustRun(t, "questions", path)

	err := run([]string{"answer", path, "1", "   "})
	if err == nil {
		t.Fatal("an empty answer was accepted")
	}

	if !strings.Contains(err.Error(), "answer is required") {
		t.Errorf("empty answer error = %q, want it to say what is missing", err)
	}

	err = run([]string{"answer", path, "7", "minor"})
	if err == nil {
		t.Fatal("an answer to a question that does not exist was accepted")
	}
}
