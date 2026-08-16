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
		// point: the block collects each under its own dimension.
		case requestMentions(req, "review this change through one dimension"):
			return callsTool("write_file", map[string]any{"path": "findings/report.json", "content": `[{"id":"F-1"}]`})

		case requestMentions(req, "try to invalidate each one"):
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
