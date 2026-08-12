package main

// The deterministic half of examples/pr-review.yml.
//
// That example cannot be a fixture: it needs a live model, an authenticated
// `gh`, and its pass/fail depends on what the reviewers find. What CAN be
// pinned is the shape it is built out of — three lenses run concurrently as
// agents, each writes its own findings file, a falsifier and gatekeeper read
// those files and narrow them, and a verdict routes the run to the end.
// That is the part that would break silently, so that is the part with a
// test.
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

// TestEndToEndPRReviewShape drives the pipeline's spine end to end: three
// lenses fan out concurrently, a falsifier and gatekeeper narrow their
// findings, and a verdict routes the run to the end.
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
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]
- name: falsifier
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]
- name: gatekeeper
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]
- name: synthesizer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]

jobs:
- name: review
  plan:
  - in_parallel:
      steps:
      - agent: reviewer
        inputs: []
        outputs: [semantic]
        prompt: Review through the semantic lens. Write semantic/findings.json.
      - agent: reviewer
        inputs: []
        outputs: [mechanical]
        prompt: Review through the mechanical lens. Write mechanical/findings.json.
      - agent: reviewer
        inputs: []
        outputs: [systemic]
        prompt: Review through the systemic lens. Write systemic/findings.json.

  - agent: falsifier
    inputs: [semantic, mechanical, systemic]
    outputs: [confirmed]
    context_paths: [semantic/findings.json, mechanical/findings.json, systemic/findings.json]
    prompt: Try to invalidate every finding above. Write confirmed/confirmed.json.

  - agent: gatekeeper
    inputs: [confirmed]
    outputs: [blocking]
    context_paths: [confirmed/confirmed.json]
    prompt: Decide what blocks the merge. Write blocking/blocking.json.

  - agent: synthesizer
    inputs: [confirmed, blocking]
    outputs: [review]
    verdicts:
      - complete: check-draft
      - blind-spots: gatekeeper    # backward: another pass at the gate
    max_visits: 2                  # which is bounded, at one extra pass
    prompt: Write the review from the confirmed findings.

  - task: check-draft
    inputs: [review]
    run: test -s review/summary.md && test -s review/findings.json
`, fake.URL+"/v1/"))

	mustRun(t, path)

	// ── all three lenses ran ─────────────────────────────────────────────────
	nodes := storeNodes(t, path)

	reviewerRuns := 0

	for _, node := range nodes {
		if node.Resource == "reviewer" {
			reviewerRuns++
		}
	}

	if reviewerRuns != 3 {
		t.Errorf("reviewer ran %d times, want 3 (one per lens)", reviewerRuns)
	}

	// ── the verdict routed the run to the end ───────────────────────────────
	// check-draft ran, so the synthesizer really did write the files it
	// claimed — and the backward blind-spots route did NOT fire, which is
	// what a second gatekeeper run would show.
	assertSucceeded(t, nodes, "task", "check-draft")

	gatekeeperRuns := 0

	for _, node := range nodes {
		if node.Resource == "gatekeeper" {
			gatekeeperRuns++
		}
	}

	if gatekeeperRuns != 1 {
		t.Errorf("gatekeeper ran %d times, want 1; the verdict was complete, so the backward blind-spots route must not have fired", gatekeeperRuns)
	}
}

// reviewScript is the model this fixture runs against: one function
// answering every agent in the pipeline, keyed by what each was asked to do.
func reviewScript() func(capturedRequest) turn {
	return func(req capturedRequest) turn {
		// The model has already done what it was told to, so it answers
		// rather than calling the same tool forever.
		if modelHasCalled(req, "write_file", "verdict") {
			return says("done")
		}

		switch {
		case requestMentions(req, "semantic lens"):
			return callsTool("write_file", map[string]any{"path": "semantic/findings.json", "content": `[{"id":"F-1","severity":"important"}]`})
		case requestMentions(req, "mechanical lens"):
			return callsTool("write_file", map[string]any{"path": "mechanical/findings.json", "content": `[]`})
		case requestMentions(req, "systemic lens"):
			return callsTool("write_file", map[string]any{"path": "systemic/findings.json", "content": `[]`})
		case requestMentions(req, "invalidate every finding"):
			return callsTool("write_file", map[string]any{"path": "confirmed/confirmed.json", "content": `[{"id":"F-1","severity":"important"}]`})
		case requestMentions(req, "decide what blocks"):
			return callsTool("write_file", map[string]any{"path": "blocking/blocking.json", "content": `[]`})
		case requestMentions(req, "write the review"):
			return callsTools(
				call("write_file", map[string]any{"path": "review/summary.md", "content": "# Review\n\none advisory finding\n"}),
				call("write_file", map[string]any{"path": "review/findings.json", "content": `[{"id":"F-1"}]`}),
				call("verdict", map[string]any{"choice": "complete"}),
			)
		}

		return says("done")
	}
}

// TestEndToEndPRReviewFanOutIsConcurrent proves the three lens cells really do
// overlap, rather than the block merely producing the right answers serially.
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

		if requestMentions(req, "lens") && !barrier.wait() {
			t.Error("a reviewer cell reached the provider alone — the cells did not overlap")
		}

		return callsTool("write_file", map[string]any{"path": "findings.json", "content": "[]"})
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [write_file]

jobs:
- name: review
  plan:
  - in_parallel:
      steps:
      - agent: reviewer
        inputs: []
        outputs: [semantic]
        prompt: Review through the semantic lens.
      - agent: reviewer
        inputs: []
        outputs: [mechanical]
        prompt: Review through the mechanical lens.
      - agent: reviewer
        inputs: []
        outputs: [systemic]
        prompt: Review through the systemic lens.
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
