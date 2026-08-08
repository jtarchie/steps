package main

// The deterministic half of examples/pr-review.yml.
//
// That example cannot be a fixture: it needs a live model, an authenticated
// `gh`, and its pass/fail depends on what the reviewers find. What CAN be
// pinned is the shape it is built out of — a step decides the width of a
// matrix, the cells run concurrently as agents, each records findings that
// survive the join under its own name, and a verdict routes the run to the
// end. That is the part that would break silently, so that is the part with a
// test.
//
// The model is scripted and routed on content rather than position:
// concurrent cells reach the provider in whatever order their goroutines are
// scheduled, so a positional script would be asserting on the interleaving
// instead of the behaviour. See newRoutedFakeLLM.

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEndToEndPRReviewShape drives the pipeline's spine end to end: compile a
// work list, fan out over it concurrently, gather what the cells found, and
// route on a verdict.
func TestEndToEndPRReviewShape(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	// Three dimensions, so the matrix is three cells wide — a number the
	// pipeline text never mentions.
	dimensions := []map[string]string{
		{"id": "state-mutation", "focus": "shared state", "scope": "store.go"},
		{"id": "api-boundaries", "focus": "exported surface", "scope": "api.go"},
		{"id": "error-paths", "focus": "error handling", "scope": "run.go"},
	}

	fake := newRoutedFakeLLM(t, reviewScript(dimensions))

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true
  context:
    fidelity: summary

agents:
- name: compiler
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
- name: falsifier
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
- name: synthesizer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools: [read_file, list_dir, write_file]

jobs:
- name: review
  plan:
  - agent: compiler
    inputs: []
    context: write
    prompt: Merge the lens proposals into one list of dimensions.

  - across:
    - var: dim
      from: dimensions
      label: id
    max_in_flight: 3
    agent: reviewer
    inputs: []
    context: write
    prompt: |
      Review this change through one dimension only: {{ .vars.dim.focus }}
      Start from: {{ .vars.dim.scope }}

  - agent: falsifier
    inputs: []
    context: write
    prompt: Every finding is a claim — try to invalidate each one.

  - agent: synthesizer
    inputs: []
    outputs: [review]
    verdicts: [complete, blind-spots]
    to:
      complete: check-draft
      blind-spots: compiler      # backward: another pass over the dimensions
    max_visits: 2                # which is bounded, at one extra pass
    prompt: Write the review from the confirmed findings.

  - task: check-draft
    inputs: [review]
    run: test -s review/summary.md && test -s review/findings.json
`, fake.URL+"/v1/"))

	mustRun(t, path)

	// ── the fan-out was as wide as the compiler said, and named by label: ───
	nodes := storeNodes(t, path)
	for _, id := range []string{"state-mutation", "api-boundaries", "error-paths"} {
		assertSucceeded(t, nodes, "agent", "reviewer [dim="+id+"]")
	}

	// ── every cell's finding survived, under a key naming the cell ──────────
	//
	// All three recorded `finding`. Serially the last would win; concurrently
	// they would race. Scoped per cell and merged at the join, all three are
	// there and each says which cell established it.
	keys := runContextKeys(t, path)
	for _, want := range []string{
		"reviewer__dim_state-mutation_.finding",
		"reviewer__dim_api-boundaries_.finding",
		"reviewer__dim_error-paths_.finding",
	} {
		if !containsString(keys, want) {
			t.Errorf("context key %q is missing; recorded: %v", want, keys)
		}
	}

	// ── the verdict routed the run to the end ───────────────────────────────
	// check-draft ran, so the synthesizer really did write the files it
	// claimed — and the backward blind-spots route did NOT fire, which is what
	// a second compiler run would show.
	assertSucceeded(t, nodes, "task", "check-draft")

	compilerRuns := 0

	for _, node := range nodes {
		if node.Resource == "compiler" {
			compilerRuns++
		}
	}

	if compilerRuns != 1 {
		t.Errorf("compiler ran %d times, want 1; the verdict was complete, so the backward blind-spots route must not have fired", compilerRuns)
	}
}

// reviewScript is the model this fixture runs against: one function answering
// every agent in the pipeline, keyed by what each was asked to do.
func reviewScript(dimensions []map[string]string) func(capturedRequest) turn {
	return func(req capturedRequest) turn {
		// The model has already done what it was told to, so it answers rather
		// than calling the same tool forever.
		//
		// Asking WHICH tool was called, rather than whether the history holds
		// any tool traffic at all: a step that reads the run context opens
		// with a synthetic read_context call-and-result pair it never made, so
		// both "there are tool results" and "the assistant called something"
		// are already true on the first turn, and either would end every
		// downstream conversation before it started.
		if modelHasCalled(req, "set_context", "write_file", "verdict") {
			return says("done")
		}

		switch {
		// The compiler decides the width of everything downstream.
		case requestMentions(req, "merge the lens proposals"):
			return callsTool("set_context", map[string]any{
				"key": "dimensions", "value": mustMarshal(dimensions),
			})

		// One reviewer cell. Every cell records the SAME key, which is the
		// point: concurrently that is a lost update unless each writes to a
		// scope only it touches and the join renames them apart.
		case requestMentions(req, "review this change through one dimension"):
			return callsTool("set_context", map[string]any{
				"key":   "finding",
				"value": "suspect behaviour under " + dimensionUnderReview(req, dimensions),
			})

		case requestMentions(req, "try to invalidate"):
			return callsTool("set_context", map[string]any{
				"key":   "confirmed",
				"value": `[{"id":"F-1","severity":"critical"}]`,
			})

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
// The provider is the observer: it holds each cell's request until it has seen
// all three. Serially the first request waits out the barrier and the test
// fails with a message saying so, rather than passing slowly.
func TestEndToEndPRReviewFanOutIsConcurrent(t *testing.T) {
	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	dir := t.TempDir()

	dimensions := []map[string]string{
		{"id": "one", "focus": "alpha", "scope": "x"},
		{"id": "two", "focus": "beta", "scope": "y"},
		{"id": "three", "focus": "gamma", "scope": "z"},
	}

	barrier := newRendezvous(len(dimensions))

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		if modelHasCalled(req, "set_context") {
			return says("done")
		}

		if requestMentions(req, "merge the lens proposals") {
			return callsTool("set_context", map[string]any{
				"key": "dimensions", "value": mustMarshal(dimensions),
			})
		}

		if requestMentions(req, "review this change") && !barrier.wait() {
			t.Error("a reviewer cell reached the provider alone — the cells did not overlap")
		}

		return says("done")
	})

	path := writePipeline(t, dir, fmt.Sprintf(`
workspace:
  strategy: copy

defaults:
  preflight:
    disabled: true

agents:
- name: compiler
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }
- name: reviewer
  source: { model: openai/test-model, endpoint: %[1]s, api_key_env: STEPS_TEST_AGENT_API_KEY }

jobs:
- name: review
  plan:
  - agent: compiler
    inputs: []
    context: write
    prompt: Merge the lens proposals into one list of dimensions.
  - across:
    - var: dim
      from: dimensions
      label: id
    max_in_flight: 3
    agent: reviewer
    inputs: []
    prompt: "Review this change: {{ .vars.dim.focus }}"
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
//
// Named rather than counted, because the runtime prepends synthetic
// call-and-result pairs of its own — a context recap arrives as a read_context
// pair the model never asked for — so "the assistant called something" is true
// before the model has done anything at all.
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

// dimensionUnderReview returns the id of whichever dimension this request's
// prompt was rendered for, so a routed provider can answer a cell in terms of
// the cell's own coordinates.
func dimensionUnderReview(req capturedRequest, dimensions []map[string]string) string {
	for _, dim := range dimensions {
		if requestMentions(req, dim["focus"]) {
			return dim["id"]
		}
	}

	return "unknown"
}

func mustMarshal(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}

	return string(data)
}

func containsString(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}

	return false
}
