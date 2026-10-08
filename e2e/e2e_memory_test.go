package e2e

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// memoryPipeline is a bot answering one message from one user: a task writes
// the user's id where the agent's memory: reads its scope, and the message is
// the prompt. Both come in as vars, so each run is a different conversation.
// outputs: and a workspace root are what make the agent step cacheable at
// all, which the last run of TestEndToEndAgentMemory depends on.
func memoryPipeline(endpoint, root string) string {
	return fmt.Sprintf(`
workspace:
  strategy: copy
  root: %s

defaults:
  preflight:
    disabled: true

agents:
- name: bot
  source:
    endpoint: %s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [remember, forget]

jobs:
- name: answer
  plan:
  - task: who
    outputs: [who]
    run: printf '%%s' '((user))' > who/user
  - agent: bot
    inputs: [who]
    outputs: [answer]
    memory:
      scope_from: who/user
    messages:
    - ((message))
`, root, endpoint)
}

var memoryIDPattern = regexp.MustCompile(`\[(\d+)\] prefers code samples`)

// TestEndToEndAgentMemory crosses the whole seam once: a file names the
// scope, a remembered fact lands in the store under it, and the NEXT run with
// the same scope opens holding it — while a run under another scope opens
// holding nothing.
//
// The fifth run is the cache-key half. It repeats the second run's exact
// inputs, so the only thing that differs is what memory holds, and a key
// blind to memory would replay the second run's answer — the one written
// while it still knew the fact.
func TestEndToEndAgentMemory(t *testing.T) {
	fake := newRoutedFakeLLM(t, memoryBot)
	path := writePipeline(t, t.TempDir(), memoryPipeline(fake.URL, t.TempDir()))
	run := memoryRunner(t, fake, path)

	first := run("U1", "I prefer code samples")

	if got := first.toolNames(); !slices.Equal(got, []string{"remember", "forget"}) {
		t.Errorf("offered tools = %v, want the two granted: recall is a preload, not a tool", got)
	}

	if first.historyCalled("recall") {
		t.Error("the first run opened with a recall exchange, with nothing remembered yet")
	}

	again := run("U1", "what do I like?")
	if !again.historyCalled("recall") || !again.toolResultContains("prefers code samples") {
		t.Errorf("U1's next run did not open with what it remembered; results = %v", again.toolResults())
	}

	other := run("U2", "what do I like?")
	if other.historyCalled("recall") || other.toolResultContains("prefers code samples") {
		t.Errorf("U2 opened with U1's memory; results = %v", other.toolResults())
	}

	run("U1", "forget that")

	forgotten := run("U1", "what do I like?")
	if forgotten.toolResultContains("prefers code samples") {
		t.Errorf("a forgotten fact came back; results = %v", forgotten.toolResults())
	}

	assertNoMemories(t, path, "U1")
}

// memoryBot remembers when told a preference, forgets by the id its preload
// showed when told to, and otherwise just answers.
func memoryBot(req capturedRequest) turn {
	switch {
	case req.userMessageContains("I prefer code samples") && !req.historyCalled("remember"):
		return callsTool("remember", map[string]any{"text": "prefers code samples"})
	case req.userMessageContains("forget that") && !req.historyCalled("forget"):
		found := memoryIDPattern.FindStringSubmatch(strings.Join(req.toolResults(), "\n"))
		if found == nil {
			return says("there is nothing to forget")
		}

		id, _ := strconv.Atoi(found[1])

		return callsTool("forget", map[string]any{"id": id})
	default:
		return says("ok")
	}
}

// memoryRunner runs the pipeline as one user saying one thing, and returns
// the run's first request — the one a preload rides on. A run that made no
// request at all was a cache hit, which every run here must not be.
func memoryRunner(t *testing.T, fake *fakeLLM, path string) func(user, message string) capturedRequest {
	t.Helper()

	return func(user, message string) capturedRequest {
		t.Helper()

		before := fake.requestCount()

		mustRun(t, "run", path, "--job", "answer", "--var", "user="+user, "--var", "message="+message)

		if fake.requestCount() == before {
			t.Fatalf("run as %s with %q made no request — a cache hit", user, message)
		}

		return fake.request(before + 1)
	}
}

func assertNoMemories(t *testing.T, path, scope string) {
	t.Helper()

	memories, err := openStoreFor(t, path).ListMemories(t.Context(), scope, 0)
	if err != nil {
		t.Fatal(err)
	}

	if len(memories) != 0 {
		t.Errorf("%s still holds %+v", scope, memories)
	}
}
