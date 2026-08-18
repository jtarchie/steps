package main

// The deterministic half of examples/pr-review.yml — run against THE FILE,
// not a copy of it.
//
// That example needs a live model, an authenticated `gh`, and a real PR, and
// its pass/fail depends on what the reviewers find. What is deterministic is
// the shape: a planner decides the width of a matrix, one reviewer cell per
// dimension runs concurrently, what they produce comes back as one collected
// artifact, a human gate parks the run, and a put posts the result.
//
// This used to be pinned against a 300-line hand-written PARAPHRASE of the
// example living in this file, which is two pipelines to keep in agreement
// and one of them untested — by the time it was replaced the paraphrase had
// grown a verdicts:/max_visits: loop the real example does not have. So the
// example itself is what runs here: read from disk, pointed at a scripted
// provider through the same source: seam the doc corpus uses, with `gh` and
// `git` stubbed on PATH so the resource type's real check/in text executes.
//
// The division of labour with the file's own assert: blocks is deliberate.
// The YAML asserts what must hold on a REAL run (each agent WROTE the
// artifact it promised); this test asserts what is only true of the fixture
// (which dimensions, how many cells, which nodes recorded).
//
// The model is scripted and routed on content rather than position:
// concurrent cells reach the provider in whatever order their goroutines are
// scheduled, so a positional script would be asserting on the interleaving
// instead of the behaviour. See newRoutedFakeLLM.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEndToEndPRReviewExample runs examples/pr-review.yml end to end.
func TestEndToEndPRReviewExample(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()
	fake := newRoutedFakeLLM(t, reviewScript())
	path := writeExampleAgainstFake(t, dir, filepath.Join("examples", "pr-review.yml"), fake.URL)

	stubGitHubCLI(t, dir)

	// Create and migrate the state db before anything races for it. Two
	// store opens in one process is the shape this test needs — the run
	// parks, a second command answers it — and SQLite's journal-to-WAL
	// conversion is not covered by busy_timeout, so a concurrent FIRST open
	// returns SQLITE_BUSY outright. Opening it once up front (this lists
	// nothing) leaves both later opens attaching to a migrated file.
	mustRun(t, "approvals", path)

	// The approval: step parks the plan, so the run has to be answered from
	// outside it — exactly as a person would, through the same command.
	done := make(chan error, 1)

	go func() {
		done <- run([]string{"test", path, "--var", "pr_repo=jtarchie/steps"})
	}()

	// Answer the gate, but never outlive the run: if the plan died before it
	// parked, the useful message is the run's own error, not "nobody was
	// waiting to be approved". stop is what keeps the poller from outliving
	// the TEST on that path — a t.Fatal below runs this defer, and a goroutine
	// still opening the store under a t.TempDir being deleted is both a
	// goleak failure and a confusing second error.
	approved := make(chan error, 1)
	stop := make(chan struct{})

	defer close(stop)

	go func() { approved <- approveWhenPending(path, stop) }()

	select {
	case err := <-done:
		t.Fatalf("the run finished without ever parking on the approval: %v", err)
	case err := <-approved:
		if err != nil {
			t.Fatalf("approving the gate: %v", err)
		}
	}

	err := <-done
	if err != nil {
		t.Fatalf("steps test %s: %v", path, err)
	}

	nodes := storeNodes(t, path)

	// The fan-out was as wide as the planner said, and named per dimension —
	// a number the example's text never mentions.
	for _, id := range []string{"state-mutation", "api-boundaries"} {
		assertSucceeded(t, nodes, "agent", "reviewer [dim="+id+"]")
	}

	// Every phase after the matrix consumed what the one before it collected,
	// through to the put — which only runs because the approval was answered.
	for _, agent := range []string{"falsifier", "gatekeeper", "synthesizer"} {
		assertSucceeded(t, nodes, "agent", agent)
	}

	assertSucceeded(t, nodes, "task", "check-draft")
	assertSucceeded(t, nodes, "put", "pr-review")

	assertCollectedFindings(t, fake)
}

// assertCollectedFindings pins the SHAPE of the collected matrix output:
// findings/<dim>/report.json per dimension, each holding the cell that wrote
// it. The example's own assert: blocks check that files exist, not where
// collection put them — so a regression that flattened or misnamed the tree
// would pass everything else. The falsifier's scripted read_file of each path
// is the observation: its tool result carries the file's content, paired to
// the call by tool_call_id in the captured request.
func assertCollectedFindings(t *testing.T, fake *fakeLLM) {
	t.Helper()

	want := map[string]string{
		"findings/state-mutation/report.json": reviewerFindings("state-mutation"),
		"findings/api-boundaries/report.json": reviewerFindings("api-boundaries"),
	}

	got := map[string]string{}

	for n := 1; n <= fake.requestCount(); n++ {
		collectReadFileResults(t, fake.request(n), want, got)
	}

	for path, content := range want {
		if got[path] != content {
			t.Errorf("collected findings at %s: got %q, want %q", path, got[path], content)
		}
	}
}

// collectReadFileResults records, into got, the content each wanted read_file
// call in req's history came back with. The fake numbers call ids per
// RESPONSE ("call_1", ...), so an id alone is ambiguous across turns of one
// conversation — walking the history in order and rebinding at each assistant
// turn scopes every tool result to the assistant message it answers.
func collectReadFileResults(t *testing.T, req capturedRequest, want, got map[string]string) {
	t.Helper()

	pending := map[string]string{}

	for _, msg := range req.Messages {
		if len(msg.ToolCalls) > 0 {
			pending = pendingReadFilePaths(msg, want)

			continue
		}

		path, ok := pending[msg.ToolCallID]
		if msg.Role != "tool" || !ok {
			continue
		}

		delete(pending, msg.ToolCallID)

		content, err := parseReadFileResult(msg.Content)
		if err != nil {
			t.Errorf("read_file %s: %v", path, err)

			continue
		}

		got[path] = content
	}
}

// pendingReadFilePaths maps each of msg's read_file call ids to the path it
// asked for, keeping only paths that appear in want.
func pendingReadFilePaths(msg capturedMessage, want map[string]string) map[string]string {
	pending := map[string]string{}

	for _, tc := range msg.ToolCalls {
		if tc.Function.Name != "read_file" {
			continue
		}

		var args struct {
			Path string `json:"path"`
		}

		if json.Unmarshal([]byte(tc.Function.Arguments), &args) != nil {
			continue
		}

		if _, wanted := want[args.Path]; wanted {
			pending[tc.ID] = args.Path
		}
	}

	return pending
}

// parseReadFileResult unpacks a read_file tool result into the file content
// it carried, surfacing the tool's own error field as an error.
func parseReadFileResult(raw string) (string, error) {
	var result struct {
		Content string `json:"content"`
		Error   string `json:"error"`
	}

	err := json.Unmarshal([]byte(raw), &result)
	if err != nil {
		return "", fmt.Errorf("not a read_file result: %w", err)
	}

	if result.Error != "" {
		return "", errors.New(result.Error)
	}

	return result.Content, nil
}

// approveWhenPending answers approval 1 once it exists, returning the last
// error if it never does. It gives up early when stop is closed.
func approveWhenPending(pipelinePath string, stop <-chan struct{}) error {
	deadline := time.Now().Add(30 * time.Second)

	var last error

	for time.Now().Before(deadline) {
		last = run([]string{"approve", pipelinePath, "1"})
		if last == nil {
			return nil
		}

		select {
		case <-stop:
			return last
		case <-time.After(20 * time.Millisecond):
		}
	}

	return last
}

// writeExampleAgainstFake copies a repo example into dir with every agent
// pointed at the fake provider — the same structural rewrite docs_test.go
// applies to a doc block, so the file on disk stays the one a reader runs.
func writeExampleAgainstFake(t *testing.T, dir, examplePath, endpoint string) string {
	t.Helper()

	body, err := os.ReadFile(examplePath) //nolint:gosec // a repo path this test names itself
	if err != nil {
		t.Fatal(err)
	}

	return writePipeline(t, dir, injectFakeProvider(t, string(body), endpoint, ""))
}

// stubGitHubCLI puts a fake `gh` and a no-op `git` at the front of PATH, so
// the example's real check:/in:/out: shell text runs against canned data
// instead of a network and an authenticated account.
//
// Stubbing the BINARIES rather than rewriting the resource type keeps the
// thing under test the text a reader would run: the `in:` script still parses
// the ref, still splits number from sha, still writes the three files the
// planner's context_paths: name.
func stubGitHubCLI(t *testing.T, dir string) {
	t.Helper()

	bin := filepath.Join(dir, "bin")

	err := os.MkdirAll(bin, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	stubs := map[string]string{
		// $2 is gh's subcommand under `pr`; every path the example takes is
		// answered, and anything else is a loud failure rather than empty
		// output that would surface much later as a missing file.
		"gh": `#!/bin/sh
case "$2" in
  list)   printf '[{"ref": "7@abc123def456"}]' ;;
  diff)   printf 'diff --git a/store.go b/store.go\n+func Set(k string) {}\n' ;;
  view)   printf '{"title":"Add Set","body":"adds a setter","files":[{"path":"store.go"}],"baseRefName":"main"}' ;;
  review) printf 'review posted\n' ;;
  *)      echo "fake gh: unexpected argv: $*" >&2; exit 1 ;;
esac
`,
		// The example clones the PR's code so reviewers can read the source.
		// The fixture's model never reads it, and a real fetch would make
		// this test need the network.
		"git": `#!/bin/sh
exit 0
`,
	}

	for name, body := range stubs {
		err = os.WriteFile(filepath.Join(bin, name), []byte(body), 0o700) //nolint:gosec // a stub this test invokes
		if err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reviewScript is the model this fixture runs against: one function
// answering every agent in the pipeline, keyed by what each was asked to do.
func reviewScript() func(capturedRequest) turn {
	return func(req capturedRequest) turn {
		// The model has already done what it was told to, so it answers
		// rather than calling the same tool forever.
		//
		// Asking WHICH tool was called, rather than whether the history holds
		// any tool traffic at all: a reviewer cell opens with a synthetic
		// read_file pair delivering its brief, so "there are tool results" is
		// already true before the model has done anything.
		if modelHasCalled(req, "write_file", "verdict") {
			return says("done")
		}

		switch {
		// The planner decides the width of everything downstream.
		case requestMentions(req, "decide the review dimensions"):
			return callsTools(
				call("write_file", map[string]any{"path": "dims/index.json", "content": `["state-mutation","api-boundaries"]`}),
				call("write_file", map[string]any{"path": "dims/state-mutation.md", "content": "look at shared state"}),
				call("write_file", map[string]any{"path": "dims/api-boundaries.md", "content": "look at the exported surface"}),
			)

		// One reviewer cell. Every cell writes the SAME path, which is the
		// point: the block collects each under its own dimension. The content
		// names the cell's dimension (read off the brief in its context), so
		// the falsifier's read below can tell a swap from a correct collection.
		case requestMentions(req, "review this change through one dimension"):
			dim := "api-boundaries"
			if strings.Contains(req.Raw, "state-mutation") {
				dim = "state-mutation"
			}

			return callsTool("write_file", map[string]any{"path": "findings/report.json", "content": reviewerFindings(dim)})

		// The falsifier reads the collected tree before writing its verdict —
		// assertCollectedFindings pairs these reads with their results to pin
		// that collection produced findings/<dim>/report.json per dimension.
		case requestMentions(req, "try to invalidate each one"):
			if !modelHasCalled(req, "read_file") {
				return callsTools(
					call("read_file", map[string]any{"path": "findings/state-mutation/report.json"}),
					call("read_file", map[string]any{"path": "findings/api-boundaries/report.json"}),
				)
			}

			return callsTool("write_file", map[string]any{"path": "confirmed/confirmed.json", "content": `[{"id":"F-1"}]`})

		case requestMentions(req, "decide whether it must be fixed"):
			return callsTool("write_file", map[string]any{"path": "blocking/blocking.json", "content": `[]`})

		case requestMentions(req, "write the review from the confirmed findings"):
			return callsTools(
				call("write_file", map[string]any{"path": "review/summary.md", "content": "# Review\n\none confirmed finding\n"}),
				call("write_file", map[string]any{"path": "review/findings.json", "content": `[{"id":"F-1"}]`}),
			)
		}

		return says("done")
	}
}

// reviewerFindings is the content the scripted reviewer cell for dim writes —
// one definition, so the script's writes and the collection assertion cannot
// drift apart.
func reviewerFindings(dim string) string {
	return fmt.Sprintf(`[{"id":"F-1","dim":%q}]`, dim)
}

// TestEndToEndPRReviewFanOutIsConcurrent proves the reviewer cells really do
// overlap, rather than the matrix merely producing the right answers serially.
//
// The provider is the observer: it holds each cell's request until it has
// seen all three. Serially the first request waits out the barrier and the
// test fails with a message saying so, rather than passing slowly.
func TestEndToEndPRReviewFanOutIsConcurrent(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	barrier := newRendezvous(3)

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if modelHasCalled(req, "write_file") {
			return says("done")
		}

		if requestMentions(req, "decide the review dimensions") {
			return callsTool("write_file", map[string]any{"path": "dims/index.json", "content": `["one","two","three"]`})
		}

		if requestMentions(req, "review through the") && !barrier.wait() {
			t.Error("a reviewer cell reached the provider alone — the cells did not overlap")
		}

		return callsTool("write_file", map[string]any{"path": "findings/report.json", "content": "[]"})
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: planner
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [write_file]
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [write_file]

jobs:
- name: review
  plan:
  - agent: planner
    inputs: []
    outputs: [dims]
    prompt: Decide the review dimensions; write dims/index.json.
  - across:
    - var: dim
      from_file: dims/index.json
    max_in_flight: 3
    agent: reviewer
    inputs: [dims]
    outputs: [findings]
    prompt: "Review through the {{ .vars.dim }} dimension."
`, fake.URL+"/v1/"))

	mustRun(t, path)
}

// rendezvous is a one-shot barrier: wait blocks until `want` callers have
// arrived, and reports false if they never do.
//
// It is how a test asserts that work OVERLAPPED rather than merely finished.
// A wall-clock comparison would be a flake generator on a loaded machine; this
// only passes if the requests are genuinely in flight together.
type rendezvous struct {
	mu     sync.Mutex
	seen   int
	want   int
	closed bool
	ready  chan struct{}
}

func newRendezvous(want int) *rendezvous {
	return &rendezvous{want: want, ready: make(chan struct{})}
}

func (r *rendezvous) wait() bool {
	r.mu.Lock()

	r.seen++
	// Guarded: a conversation that takes more turns than expected would
	// otherwise close an already-closed channel and panic inside an HTTP
	// handler, which reads as a hang rather than as a test failure.
	if r.seen >= r.want && !r.closed {
		r.closed = true
		close(r.ready)
	}

	r.mu.Unlock()

	select {
	case <-r.ready:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

// modelHasCalled reports whether this conversation already contains an
// assistant turn requesting one of the named tools.
func modelHasCalled(req capturedRequest, names ...string) bool {
	for _, msg := range req.Messages {
		if msg.Role != "assistant" {
			continue
		}

		for _, called := range msg.ToolCalls {
			for _, name := range names {
				if called.Function.Name == name {
					return true
				}
			}
		}
	}

	return false
}

// requestMentions reports whether any user message in the request contains
// text, case-insensitively — the seam that lets one scripted provider serve a
// pipeline whose agents are each told something different.
func requestMentions(req capturedRequest, text string) bool {
	for _, msg := range req.Messages {
		if msg.Role == "user" && strings.Contains(strings.ToLower(msg.Content), strings.ToLower(text)) {
			return true
		}
	}

	return false
}
