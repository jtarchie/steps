package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file is the end-to-end test harness: a scriptable stand-in for the
// OpenAI-compatible endpoint an agent step talks to, plus assertion helpers
// for the layers a run passes through on the way there and back.
//
// It exists because agent.RunStep builds its LLM client internally (there is
// no injectable model.LLM at the CLI boundary — a deliberate credential
// boundary, see docs/agents.md), so the only seam for a full-stack test is
// the HTTP endpoint itself. Pointing an agent's source.endpoint: at an
// httptest.Server is therefore the *only* way to exercise CLI → config →
// merkle → resource → workspace → agent-conversation → route → store as one
// pass. The pre-existing agent tests each hand-rolled a single-response
// server inline; this generalizes that into a scripted, recording one so a
// multi-turn tool-calling conversation can be driven and inspected.
//
// Nothing here needs the network, docker, or an API key: newFakeLLM is the
// whole provider.

// scriptedCall is one tool call the fake makes the model request. args is
// marshaled into the OpenAI `arguments` string verbatim, so a test can send
// a deliberately wrong shape to exercise the tool layer's own validation.
type scriptedCall struct {
	name string
	args map[string]any
}

// Why so many fixtures in this package carry
// `defaults: {preflight: {disabled: true}}`: `steps run` probes every model a
// job reaches before running any step (see internal/pipeline's preflight), and
// that probe is a real provider request. Left on, it would pop a scripted turn
// and shift every request count in every test that is about something else.
// preflight_test.go is where the probe itself is exercised, deliberately.

// turn is one scripted provider response, popped in order — one per request
// the run makes. Exactly one of its modes applies: a plain assistant message
// (text), a tool-call message (calls), an HTTP error (status), or a verbatim
// body (body, for malformed-response cases).
type turn struct {
	text   string
	calls  []scriptedCall
	status int
	body   string
	// usage, when non-zero, adds the provider's own token accounting to the
	// response. Real providers report it on every completion; scripting it is
	// what lets a test drive budget: enforcement, which counts reported usage
	// and never an estimate.
	usage int
}

// spending returns tn with the provider reporting total tokens for it.
func (tn turn) spending(total int) turn {
	tn.usage = total

	return tn
}

// usageJSON renders the usage block, split across prompt and completion the
// way a real completion does. Empty when the turn scripts no usage, so the
// no-usage-reported path stays exercised too.
func (tn turn) usageJSON() string {
	if tn.usage == 0 {
		return ""
	}

	prompt := tn.usage * 8 / 10

	return fmt.Sprintf(`,"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`,
		prompt, tn.usage-prompt, tn.usage)
}

// says scripts a plain assistant reply — the message that ends a
// conversation, provided every required tool has already been satisfied.
func says(text string) turn {
	return turn{text: text}
}

// callsTool scripts an assistant turn requesting one tool call.
func callsTool(name string, args map[string]any) turn {
	return turn{calls: []scriptedCall{{name: name, args: args}}}
}

// callsTools scripts an assistant turn requesting several tool calls at once
// — the parallel-tool-call shape a real provider emits, which the
// conversation loop must execute in order and answer with one tool-result
// message per call.
func callsTools(calls ...scriptedCall) turn {
	return turn{calls: calls}
}

// call is one entry for callsTools.
func call(name string, args map[string]any) scriptedCall {
	return scriptedCall{name: name, args: args}
}

// failsWith scripts a transport-level failure: the endpoint answers with an
// HTTP status instead of a completion. Used to prove an LLM outage
// classifies as errored (infrastructure) rather than failed (task-level),
// and — for a non-retryable status — that fallback:'s mid-run cascade
// leaves a model's own rejection alone.
func failsWith(status int) turn {
	return turn{status: status}
}

// render writes this turn as an OpenAI chat-completion response. The shape
// mirrors what a real provider sends, verified against the client in use
// (adk-utils-go's genai/openai adapter, non-streaming): an assistant message
// carries either content or tool_calls, and finish_reason distinguishes them.
func (tn turn) render(w http.ResponseWriter) {
	if tn.status != 0 {
		http.Error(w, "scripted provider failure", tn.status)

		return
	}

	w.Header().Set("Content-Type", "application/json")

	if tn.body != "" {
		_, _ = io.WriteString(w, tn.body)

		return
	}

	if len(tn.calls) == 0 {
		_, _ = fmt.Fprintf(w, `{"id":"fake","object":"chat.completion","created":0,"model":"test-model",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":%s}}]%s}`,
			mustJSON(tn.text), tn.usageJSON())

		return
	}

	calls := make([]string, 0, len(tn.calls))

	for i, call := range tn.calls {
		args, err := json.Marshal(call.args)
		if err != nil {
			args = []byte("{}")
		}

		calls = append(calls, fmt.Sprintf(
			`{"id":"call_%d","type":"function","function":{"name":%s,"arguments":%s}}`,
			i+1, mustJSON(call.name), mustJSON(string(args))))
	}

	_, _ = fmt.Fprintf(w, `{"id":"fake","object":"chat.completion","created":0,"model":"test-model",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[%s]}}]%s}`,
		strings.Join(calls, ","), tn.usageJSON())
}

// mustJSON renders v as a JSON literal for embedding in a response template.
// Only ever called with strings the test itself authored, so a marshal
// failure is impossible; it degrades to an empty string rather than panicking
// inside an HTTP handler goroutine.
func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return `""`
	}

	return string(data)
}

// capturedToolCall is one tool call as it appeared on the wire in a request's
// message history — i.e. what the conversation loop echoed back to the model.
type capturedToolCall struct {
	ID       string               `json:"id"`
	Function capturedCallFunction `json:"function"`
}

type capturedCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// capturedMessage is one message from a captured request's history. content
// arrives as a JSON string (or null on an assistant tool-call message, which
// unmarshals to "").
type capturedMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content"`
	ToolCallID string             `json:"tool_call_id"`
	ToolCalls  []capturedToolCall `json:"tool_calls"`
}

type capturedToolFunction struct {
	Name string `json:"name"`
}

type capturedTool struct {
	Function capturedToolFunction `json:"function"`
}

// capturedRequest is one parsed request the run sent the provider. It is the
// wire-layer assertion surface: which tools were compiled and offered, what
// the system message said, what tool results were fed back, and whether a
// required tool was being forced.
type capturedRequest struct {
	Model      string            `json:"model"`
	Messages   []capturedMessage `json:"messages"`
	Tools      []capturedTool    `json:"tools"`
	ToolChoice json.RawMessage   `json:"tool_choice"`
	Raw        string            `json:"-"`
}

// toolNames returns the names of every tool offered on this request, in
// order — the compiled result of the agent's tools: grant plus anything
// synthesized (a verdict tool, a sub-agent tool).
func (r capturedRequest) toolNames() []string {
	names := make([]string, 0, len(r.Tools))

	for _, tool := range r.Tools {
		names = append(names, tool.Function.Name)
	}

	return names
}

// systemMessage returns the request's system prompt, or "" if it has none.
func (r capturedRequest) systemMessage() string {
	for _, msg := range r.Messages {
		if msg.Role == "system" {
			return msg.Content
		}
	}

	return ""
}

// toolResults returns the content of every tool-result message in the
// request's history, in order — what the agent's tool execution actually fed
// back to the model.
func (r capturedRequest) toolResults() []string {
	var results []string

	for _, msg := range r.Messages {
		if msg.Role == "tool" {
			results = append(results, msg.Content)
		}
	}

	return results
}

// userMessageContains reports whether any user message in the request's
// history carries text — how a routed fake pins that a prompt (inline, from a
// file, or from a fetched artifact) actually reached the wire.
func (r capturedRequest) userMessageContains(text string) bool {
	for _, msg := range r.Messages {
		if msg.Role == "user" && strings.Contains(msg.Content, text) {
			return true
		}
	}

	return false
}

// toolResultContains reports whether any tool result in the request's history
// carries text — how a routed fake gates its answer on a tool having actually
// delivered content, rather than merely having been requested.
func (r capturedRequest) toolResultContains(text string) bool {
	for _, result := range r.toolResults() {
		if strings.Contains(result, text) {
			return true
		}
	}

	return false
}

// historyCalled reports whether the request's history carries an assistant
// tool call of this name — the other half of "a completed tool turn is
// present", alongside toolResults.
func (r capturedRequest) historyCalled(name string) bool {
	for _, msg := range r.Messages {
		for _, call := range msg.ToolCalls {
			if call.Function.Name == name {
				return true
			}
		}
	}

	return false
}

// forcedTool returns the name of the tool this request's tool_choice forces,
// or "" when the model was left free to choose. A non-empty value is the
// observable signal that the conversation loop caught the model trying to
// stop with a required tool unsatisfied (see forceRequiredTool).
func (r capturedRequest) forcedTool() string {
	if len(r.ToolChoice) == 0 {
		return ""
	}

	var choice struct {
		Function capturedToolFunction `json:"function"`
	}

	err := json.Unmarshal(r.ToolChoice, &choice)
	if err != nil {
		return ""
	}

	return choice.Function.Name
}

// fakeLLM is a scripted, recording OpenAI-compatible endpoint. Point an
// agent's source.endpoint: at URL + "/v1/".
type fakeLLM struct {
	URL string

	t      *testing.T
	mu     sync.Mutex
	script []turn
	repeat bool
	next   int
	// route, when set, answers each request from its CONTENT instead of its
	// position. See newRoutedFakeLLM.
	route    func(capturedRequest) turn
	requests []capturedRequest
	// overrun suppresses the "script exhausted" test failure, still
	// answering 500. Set only by the mutation harness (dsl_mutation_test.go),
	// where a mutant is EXPECTED to make a step fail and retry: an extra
	// request there is the mutation working, not the fixture being wrong.
	overrun bool
}

// tolerateOverrun returns a scenario whose provider does not fail the test
// when a run asks for more turns than the script holds.
func (s docScenario) tolerateOverrun() docScenario {
	if s.fake == nil {
		return s
	}

	inner := s.fake

	s.fake = func(t *testing.T) *fakeLLM {
		t.Helper()

		fake := inner(t)

		fake.mu.Lock()
		fake.overrun = true
		fake.mu.Unlock()

		return fake
	}

	return s
}

// newFakeLLM starts a fake provider that answers the run's requests with
// script, in order. A request past the end of the script fails the test
// rather than hanging or silently looping — an agent that asks for more
// turns than the fixture anticipated is itself a regression worth catching.
func newFakeLLM(t *testing.T, script ...turn) *fakeLLM {
	t.Helper()

	return startFakeLLM(t, false, script)
}

// newRepeatingFakeLLM starts a fake provider that answers every request with
// the same turn, however many the run makes. For tests whose subject is how
// often (or whether) the agent was reached rather than what it was told —
// there is no script to exhaust, so requestCount is the assertion.
func newRepeatingFakeLLM(t *testing.T, only turn) *fakeLLM {
	t.Helper()

	return startFakeLLM(t, true, []turn{only})
}

// newRoutedFakeLLM starts a fake provider that answers each request from what
// it ASKS rather than from its position in a script.
//
// Position stops being a usable key the moment a pipeline runs agents
// concurrently: `max_in_flight:` cells reach the provider in whatever order
// their goroutines get scheduled, so turn 3 belongs to whichever cell got
// there first, which is exactly what a fixture must not depend on. Routing on
// content is order-independent, so the same assertions hold however the run
// interleaves.
//
// route runs on the server's goroutines and must therefore be safe to call
// concurrently — read the request, return a turn, keep no state.
func newRoutedFakeLLM(t *testing.T, route func(capturedRequest) turn) *fakeLLM {
	t.Helper()

	fake := startFakeLLM(t, false, nil)
	fake.route = route

	return fake
}

func startFakeLLM(t *testing.T, repeat bool, script []turn) *fakeLLM {
	t.Helper()

	fake := &fakeLLM{t: t, script: script, repeat: repeat}

	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)

	fake.URL = server.URL

	return fake
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("fake provider: read request body: %v", err)
		http.Error(w, "unreadable body", http.StatusBadRequest)

		return
	}

	req := capturedRequest{Raw: string(data)}

	err = json.Unmarshal(data, &req)
	if err != nil {
		f.t.Errorf("fake provider: request is not valid JSON: %v\n%s", err, data)
		http.Error(w, "unparseable body", http.StatusBadRequest)

		return
	}

	f.mu.Lock()
	f.requests = append(f.requests, req)
	index := f.next
	f.next++
	route := f.route
	overrun := f.overrun
	f.mu.Unlock()

	if route != nil {
		route(req).render(w)

		return
	}

	if index >= len(f.script) {
		if f.repeat {
			f.script[len(f.script)-1].render(w)

			return
		}

		// t.Errorf (not Fatalf) — this runs on the server's goroutine, where
		// FailNow would abandon the handler mid-response and leave the run
		// blocked on a reply that never comes.
		if !overrun {
			f.t.Errorf("fake provider: request %d has no scripted turn (script has %d)", index+1, len(f.script))
		}

		http.Error(w, "script exhausted", http.StatusInternalServerError)

		return
	}

	f.script[index].render(w)
}

// requestCount is how many requests the run made to the provider.
func (f *fakeLLM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

// request returns the 1-indexed nth captured request, failing the test if
// the run never made it. 1-indexed to match how the scripts and assertions
// read ("on the second turn the model...").
func (f *fakeLLM) request(n int) capturedRequest {
	f.t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	if n < 1 || n > len(f.requests) {
		f.t.Fatalf("no request %d: the run made %d provider request(s)", n, len(f.requests))
	}

	return f.requests[n-1]
}

// nodeRow is one row of the store's nodes table: a step's recorded outcome.
type nodeRow struct {
	Kind      string
	Resource  string
	StepIndex int
	Status    string
	Error     string
}

// jobRunRow is one row of the store's job_runs table: a chain's recorded
// outcome, which is what a later run consults to decide whether to skip.
type jobRunRow struct {
	JobName string
	Status  string
	Error   string
}

// openStateDB opens the .steps/state.db colocated with a pipeline YAML, for
// reading. It fails the test when the database doesn't exist — sql.Open
// would happily create an empty one, turning "the run recorded nothing" into
// a silently passing assertion.
func openStateDB(t *testing.T, pipelinePath string) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(filepath.Dir(pipelinePath), ".steps", "state.db")

	_, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("no state db at %s: %v", dbPath, err)
	}

	// The "sqlite" driver is registered transitively via internal/store,
	// which main imports — package main may not import modernc.org/sqlite
	// directly (see .golangci.yml's depguard Main rule).
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open state db: %v", err)
	}

	t.Cleanup(func() {
		_ = db.Close()
	})

	return db
}

// storeTranscript returns the persisted conversation transcript JSON for the
// node with this resource name, "" when none was recorded — the on-demand
// record RunStep saves alongside nodes.result (see docs/agents-internals.md,
// "Transcript persistence").
func storeTranscript(t *testing.T, pipelinePath, resource string) string {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	var transcript string

	err := db.QueryRowContext(t.Context(), `
		SELECT nt.transcript FROM node_transcripts nt
		JOIN nodes n ON n.hash = nt.hash
		WHERE n.resource = ?`, resource).Scan(&transcript)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}

	if err != nil {
		t.Fatalf("query transcript for %q: %v", resource, err)
	}

	return transcript
}

// storeNodeResult returns the most recent nodes.result JSON recorded for a
// step, "" when the step recorded none.
func storeNodeResult(t *testing.T, pipelinePath, resource string) string {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	var result string

	err := db.QueryRowContext(t.Context(), `
		SELECT COALESCE(result, '') FROM nodes
		WHERE resource = ? ORDER BY created_at DESC LIMIT 1`, resource).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}

	if err != nil {
		t.Fatalf("query result for %q: %v", resource, err)
	}

	return result
}

// storeNodes returns every node the run recorded, in step order — the
// persistence layer's view of what executed and how it ended.
func storeNodes(t *testing.T, pipelinePath string) []nodeRow {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(), `SELECT kind, resource, step_index, status, COALESCE(error, '') FROM nodes ORDER BY step_index, created_at`)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var nodes []nodeRow

	for rows.Next() {
		var node nodeRow

		err = rows.Scan(&node.Kind, &node.Resource, &node.StepIndex, &node.Status, &node.Error)
		if err != nil {
			t.Fatalf("scan node: %v", err)
		}

		nodes = append(nodes, node)
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("iterate nodes: %v", err)
	}

	return nodes
}

// storeJobRuns returns every recorded chain outcome.
func storeJobRuns(t *testing.T, pipelinePath string) []jobRunRow {
	t.Helper()

	db := openStateDB(t, pipelinePath)

	rows, err := db.QueryContext(t.Context(), `SELECT job_name, status, COALESCE(error, '') FROM job_runs ORDER BY created_at`)
	if err != nil {
		t.Fatalf("query job_runs: %v", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var runs []jobRunRow

	for rows.Next() {
		var run jobRunRow

		err = rows.Scan(&run.JobName, &run.Status, &run.Error)
		if err != nil {
			t.Fatalf("scan job_run: %v", err)
		}

		runs = append(runs, run)
	}

	err = rows.Err()
	if err != nil {
		t.Fatalf("iterate job_runs: %v", err)
	}

	return runs
}

// findNode returns the recorded node for a given kind and resource/task
// name. Steps are addressed by name rather than index so a fixture can grow
// a step without renumbering every assertion.
func findNode(t *testing.T, nodes []nodeRow, kind, resource string) nodeRow {
	t.Helper()

	for _, node := range nodes {
		if node.Kind == kind && node.Resource == resource {
			return node
		}
	}

	t.Fatalf("no %s node recorded for %q; recorded: %+v", kind, resource, nodes)

	return nodeRow{}
}

// writePipeline writes a pipeline YAML into dir and returns its path.
func writePipeline(t *testing.T, dir, yaml string) string {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatalf("write pipeline: %v", err)
	}

	return path
}

// readFileString returns path's contents, failing the test if it's missing.
// Used to assert on the side effects a pipeline's own shell commands left in
// the test's directory — the only durable view of a run's filesystem work,
// since step workspaces are torn down when the build closes.
func readFileString(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped file the test itself arranged
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}

	return string(data)
}

// assertNoFile fails the test if path exists — the way to prove a step did
// NOT run, given each step's side effect is a file it appends to.
func assertNoFile(t *testing.T, path string) {
	t.Helper()

	_, err := os.Stat(path)
	if err == nil {
		t.Errorf("%s exists, but the step that writes it should not have run", filepath.Base(path))
	}
}

// captureStderr redirects os.Stderr to a temp file for the rest of the test
// and returns a reader for what was written. The logger writes there
// (initLogging), and run() installs its own handler, so an in-process
// slog.SetDefault would be overwritten the moment the CLI starts — the file
// swap is what lets a test assert on what an operator would actually see.
func captureStderr(t *testing.T) func() string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "stderr.log")

	file, err := os.Create(path) //nolint:gosec // path is under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	prev := os.Stderr
	os.Stderr = file

	t.Cleanup(func() {
		os.Stderr = prev
		_ = file.Close()
	})

	return func() string {
		body, readErr := os.ReadFile(path) //nolint:gosec // path is under t.TempDir()
		if readErr != nil {
			t.Fatal(readErr)
		}

		return string(body)
	}
}
