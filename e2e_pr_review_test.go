package main

// The deterministic half of examples/pr-review.yml.
//
// That example cannot be a fixture: it needs a live model, an authenticated
// `gh`, and its pass/fail depends on what the reviewers find. What CAN be
// pinned is the shape it is built out of — a planner step decides the width
// of a matrix, one reviewer cell per dimension runs concurrently, everything
// they produce comes back as one collected artifact, and a verdict routes the
// run to the end. That is the part that would break silently, so that is the
// part with a test.
//
// The model is scripted and routed on content rather than position:
// concurrent cells reach the provider in whatever order their goroutines are
// scheduled, so a positional script would be asserting on the interleaving
// instead of the behaviour. See newRoutedFakeLLM.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEndToEndPRReviewShape drives the pipeline's spine end to end: a planner
// writes the work list, the matrix fans out over it concurrently, the cells'
// findings come back as one artifact, and a verdict routes the run to the end.
func TestEndToEndPRReviewShape(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	fake := newRoutedFakeLLM(t, reviewScript())

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: planner
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, write_file]
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, write_file]
- name: falsifier
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]
- name: synthesizer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]

jobs:
- name: review
  plan:
  - agent: planner
    inputs: []
    outputs: [dims]
    prompt: Decide the review dimensions; write dims/index.json and one brief per id.

  - across:
    - var: dim
      from_file: dims/index.json
    max_in_flight: 2
    try:
      agent: reviewer
      inputs: [dims]
      outputs: [findings]
      context_paths: ["dims/{{ .vars.dim }}.md"]
      prompt: Review through the {{ .vars.dim }} dimension; write findings/report.json.

  - agent: falsifier
    inputs: [findings]
    outputs: [confirmed]
    prompt: Invalidate every finding under findings/; write confirmed/confirmed.json.

  - agent: synthesizer
    inputs: [confirmed]
    outputs: [review]
    verdicts:
      - complete: check-draft
      - blind-spots: falsifier     # backward: another pass at the gate
    max_visits: 2                  # which is bounded, at one extra pass
    prompt: Write the review from the confirmed findings.

  - task: check-draft
    inputs: [review]
    run: test -s review/summary.md && test -s review/findings.json
`, fake.URL+"/v1/"))

	mustRun(t, path)

	// ── the fan-out was as wide as the planner said, and named per dimension —
	// a number the pipeline text never mentions.
	nodes := storeNodes(t, path)
	for _, id := range []string{"state-mutation", "api-boundaries"} {
		assertSucceeded(t, nodes, "agent", "reviewer [dim="+id+"]")
	}

	// ── the verdict routed the run to the end ───────────────────────────────
	// check-draft ran, so the synthesizer really did write the files it
	// claimed — and the backward blind-spots route did NOT fire, which is
	// what a second falsifier run would show.
	assertSucceeded(t, nodes, "task", "check-draft")

	falsifierRuns := 0

	for _, node := range nodes {
		if node.Resource == "falsifier" {
			falsifierRuns++
		}
	}

	if falsifierRuns != 1 {
		t.Errorf("falsifier ran %d times, want 1; the verdict was complete, so the backward blind-spots route must not have fired", falsifierRuns)
	}
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
		case requestMentions(req, "review through the"):
			return callsTool("write_file", map[string]any{"path": "findings/report.json", "content": `[{"id":"F-1"}]`})

		case requestMentions(req, "invalidate every finding"):
			return callsTool("write_file", map[string]any{"path": "confirmed/confirmed.json", "content": `[{"id":"F-1"}]`})

		case requestMentions(req, "write the review"):
			return callsTools(
				call("write_file", map[string]any{"path": "review/summary.md", "content": "# Review\n\none confirmed finding\n"}),
				call("write_file", map[string]any{"path": "review/findings.json", "content": `[{"id":"F-1"}]`}),
				call("verdict", map[string]any{"choice": "complete"}),
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
