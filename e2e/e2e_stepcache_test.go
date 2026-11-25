package e2e

// End-to-end coverage for the step output cache: the claim that an unchanged
// expensive step costs nothing the second time, and that what it produced is
// still there for the steps below it.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stepCachePipeline is a plan whose middle step is the expensive one: a get
// feeds an agent, and a task downstream reads what the agent wrote.
//
// The agent declares no verdicts:, no to:, no hooks and no context: from:, and
// declares outputs — the shape the whole pr-review fan-out has, and the only
// step here the cache may reuse.
//
// The two tasks below it re-run on every pass for two different reasons, and
// both matter. publish declares an output, so it is cacheable on paper and
// runs only because it is volatile: — without that, a restored report/ nobody
// reads would prove nothing about restoration. notify declares NO outputs, so
// nothing about it is reusable: a hit would have nothing to restore and would
// simply drop the side effect its run: exists for.
//
// One fake provider serves every run of a test, and its URL is baked in here:
// the endpoint is part of an agent's hashed content, so a second provider on a
// second port would be a different step and would miss the cache for a reason
// that has nothing to do with what is under test.
func stepCachePipeline(t *testing.T, dir, endpoint, publishLog, fetchLog, notifyLog, notes string) string {
	t.Helper()

	return writePipeline(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: |
      echo '%[4]s' > NOTES.txt
      echo fetched >> %[3]s

resources:
- name: repo
  type: dummy
  source: {}

agents:
- name: reviewer
  source:
    endpoint: %[1]s/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY
  tools: [read_file, write_file]

jobs:
- name: build
  plan:
  - get: repo
  - agent: reviewer
    inputs: [repo]
    outputs: [report]
    messages:
      - Review the notes and summarize them.
  - task: publish
    volatile: true
    inputs: [report]
    outputs: [published]
    run: |
      cat report/summary.md >> %[2]s
      echo >> %[2]s
      cp report/summary.md published/
  - task: notify
    run: echo notified >> %[5]s
  assert:
    execution: [repo, reviewer, publish, notify]
    outcome: succeeded
`, endpoint, publishLog, fetchLog, notes, notifyLog))
}

// stepCacheScript is the conversation the reviewer has when it actually runs.
func stepCacheScript() []turn {
	return []turn{
		callsTool("read_file", map[string]any{"path": "repo/NOTES.txt"}),
		callsTool("write_file", map[string]any{
			"path":    "report/summary.md",
			"content": "widgetd seeds its catalog from widgets.json.",
		}),
		says("Done."),
	}
}

// stepCacheScriptTimes repeats the script, for a test whose agent is expected
// to run more than once against one provider.
func stepCacheScriptTimes(n int) []turn {
	script := make([]turn, 0, n*len(stepCacheScript()))
	for range n {
		script = append(script, stepCacheScript()...)
	}

	return script
}

const stepCacheNotes = "The widget catalog is seeded from widgets.json on boot."

// TestStepCacheReusesAnAgentStep is the feature's headline claim: rerunning a
// pipeline whose inputs did not change costs no model calls, and the artifact
// the reused step produced is still delivered to the step that reads it.
func TestStepCacheReusesAnAgentStep(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")
	notifyLog := filepath.Join(dir, "notify.log")

	fake := newFakeLLM(t, stepCacheScript()...)
	path := stepCachePipeline(t, dir, fake.URL, publishLog, fetchLog, notifyLog, stepCacheNotes)

	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != len(stepCacheScript()) {
		t.Fatalf("provider requests on the first run = %d, want %d", got, len(stepCacheScript()))
	}

	if got := readFileString(t, publishLog); !strings.Contains(got, "widgetd seeds its catalog") {
		t.Fatalf("the first run's publish step did not see the agent's report; got %q", got)
	}

	// Second pass, same root: the agent's content and the bytes of its one
	// declared input are unchanged, so its entry is found and no model is
	// asked anything. The script is exhausted, so a request would also fail
	// the fake — the count is the assertion that says why.
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != len(stepCacheScript()) {
		t.Errorf("provider requests after the rerun = %d, want %d — the rerun asked the model %d more time(s)",
			got, len(stepCacheScript()), got-len(stepCacheScript()))
	}

	// The volatile: task ran again, and the report it read came out of the
	// cache rather than out of a conversation.
	assertLineCount(t, publishLog, 2)

	if got := readFileString(t, publishLog); strings.Count(got, "widgetd seeds its catalog") != 2 {
		t.Errorf("the reused agent step's report did not reach the second run's publish step; got %q", got)
	}

	// The get's version is unchanged, so the resource cache, always on under a
	// durable root as Concourse's is, hands the second build the tree it
	// fetched rather than running in: again.
	assertLineCount(t, fetchLog, 1)

	// The output-less task ran both times. A step with nothing to restore must
	// never be "reused": that is not reuse, it is dropping whatever its run:
	// actually did — a webhook, a deploy, a notification.
	assertLineCount(t, notifyLog, 2)

	// The reused agent node says so. Without this a succeeded agent node with
	// no transcript and no tokens is indistinguishable from one that ran and
	// produced nothing.
	assertSucceeded(t, storeNodes(t, path), "agent", "reviewer")

	if got := storeNodeResult(t, path, "reviewer"); !strings.Contains(got, `"reused":true`) {
		t.Errorf("the reused agent node's result = %q, want it to record that it was reused", got)
	}
}

// TestStepCacheRerunsWhenAnInputChanges is the other half of the contract: the
// key is the input BYTES, so a changed upstream artifact re-runs the step even
// though the step's own declaration is untouched.
func TestStepCacheRerunsWhenAnInputChanges(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")
	notifyLog := filepath.Join(dir, "notify.log")

	fake := newFakeLLM(t, stepCacheScriptTimes(2)...)

	mustRun(t, "test", "--workspace-root", root, stepCachePipeline(t, dir, fake.URL, publishLog, fetchLog, notifyLog, stepCacheNotes))

	// Same plan, same agent, same endpoint — only what the get: fetches is
	// different, which is a change no plan-time hash of the agent step can
	// see.
	changed := "The widget catalog is seeded from catalog.toml on boot."
	mustRun(t, "test", "--workspace-root", root, stepCachePipeline(t, dir, fake.URL, publishLog, fetchLog, notifyLog, changed))

	if got := fake.requestCount(); got != 2*len(stepCacheScript()) {
		t.Errorf("provider requests across both runs = %d, want %d (different input bytes are different work)",
			got, 2*len(stepCacheScript()))
	}
}

// TestStepCacheRerunsWhenThePromptChanges is the key half the input-bytes test
// cannot reach: same inputs, same endpoint, a different message is different
// work. The unchanged rerun is proven cached first, so the request that
// follows can only be the edited prompt.
func TestStepCacheRerunsWhenThePromptChanges(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	fake := newFakeLLM(t, stepCacheScriptTimes(2)...)

	path := stepCachePipeline(t, dir, fake.URL,
		filepath.Join(dir, "publish.log"), filepath.Join(dir, "fetch.log"), filepath.Join(dir, "notify.log"), stepCacheNotes)

	mustRun(t, "test", "--workspace-root", root, path)
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != len(stepCacheScript()) {
		t.Fatalf("provider requests after an unchanged rerun = %d, want %d — the step was not cached, so nothing below proves anything", got, len(stepCacheScript()))
	}

	rewritePipeline(t, path, "Review the notes and summarize them.", "Review the notes and list every file they name.")
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != 2*len(stepCacheScript()) {
		t.Errorf("provider requests after the prompt changed = %d, want %d — the cached step was served for a prompt it never ran", got, 2*len(stepCacheScript()))
	}
}

// TestStepCacheVolatileAgentAlwaysRuns pins volatile: on the kind of step it
// exists for — one whose answer must never be replayed.
func TestStepCacheVolatileAgentAlwaysRuns(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	publishLog := filepath.Join(dir, "publish.log")
	fetchLog := filepath.Join(dir, "fetch.log")
	notifyLog := filepath.Join(dir, "notify.log")

	fake := newFakeLLM(t, stepCacheScriptTimes(2)...)

	path := stepCachePipeline(t, dir, fake.URL, publishLog, fetchLog, notifyLog, stepCacheNotes)
	volatileAgent := strings.Replace(
		readFileString(t, path),
		"  - agent: reviewer\n",
		"  - agent: reviewer\n    volatile: true\n",
		1,
	)
	path = writePipeline(t, dir, volatileAgent)

	mustRun(t, "test", "--workspace-root", root, path)
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != 2*len(stepCacheScript()) {
		t.Errorf("provider requests across both runs = %d, want %d (volatile: must never be reused)",
			got, 2*len(stepCacheScript()))
	}
}

// rewritePipeline swaps one spelling for another in a pipeline already on disk. Same file, same path, same state database — so the only thing that differs between two runs is the swapped text.
func rewritePipeline(t *testing.T, path, from, to string) {
	t.Helper()

	source := readFileString(t, path)
	if !strings.Contains(source, from) {
		t.Fatalf("%s does not contain %q", path, from)
	}

	err := os.WriteFile(path, []byte(strings.Replace(source, from, to, 1)), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

// TestStepCacheRerunsWhenAGrantsTermsChange is the end-to-end half of internal/merkle's keyfields test, for the two grant fields that were found outside the key: a cached agent step was served as-is after its tool became required:, and after its web_fetch allow-list changed. Each run here is proven cached FIRST, so the re-run that follows can only be the changed term.
func TestStepCacheRerunsWhenAGrantsTermsChange(t *testing.T) {
	t.Parallel()

	script := []turn{
		callsTool("read_file", map[string]any{"path": "repo/NOTES.txt"}),
		callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "widgetd seeds its catalog from widgets.json."}),
		callsTool("lint", map[string]any{}),
		says("Done."),
	}

	const lint = "  - name: lint\n    description: lints the report\n    run: \"true\"\n"

	for name, grants := range map[string][2]string{
		"a custom tool becomes required": {
			"  tools:\n  - read_file\n  - write_file\n" + lint,
			"  tools:\n  - read_file\n  - write_file\n" + lint + "    required: true\n",
		},
		"a web_fetch allow-list widens": {
			"  tools:\n  - read_file\n  - write_file\n" + lint + "  - builtin: web_fetch\n    allow: [docs.example]\n",
			"  tools:\n  - read_file\n  - write_file\n" + lint + "  - builtin: web_fetch\n    allow: [docs.example, evil.example]\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			fake := newFakeLLM(t, append(append([]turn{}, script...), script...)...)

			path := stepCachePipeline(t, dir, fake.URL,
				filepath.Join(dir, "publish.log"), filepath.Join(dir, "fetch.log"), filepath.Join(dir, "notify.log"), stepCacheNotes)

			rewritePipeline(t, path, "  tools: [read_file, write_file]\n", grants[0])
			mustRun(t, "test", "--workspace-root", root, path)
			mustRun(t, "test", "--workspace-root", root, path)

			if got := fake.requestCount(); got != len(script) {
				t.Fatalf("provider requests after an unchanged rerun = %d, want %d — the step was not cached, so nothing below proves anything", got, len(script))
			}

			rewritePipeline(t, path, grants[0], grants[1])
			mustRun(t, "test", "--workspace-root", root, path)

			if got := fake.requestCount(); got != 2*len(script) {
				t.Errorf("provider requests after the grant changed = %d, want %d — the cached step was served under terms it never ran with", got, 2*len(script))
			}
		})
	}
}

// A nudge can turn a run that would have failed into a success, so a step cached under nudge: true must not stand in for the same step without it: removing the nudge to prove a prompt works unaided has to ask the model. Proven cached FIRST, so the re-run can only be the removed nudge.
func TestStepCacheRerunsWhenTheNudgeIsRemoved(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	script := stepCacheScript()
	fake := newFakeLLM(t, append(stepCacheScript(), script...)...)

	path := stepCachePipeline(t, dir, fake.URL,
		filepath.Join(dir, "publish.log"), filepath.Join(dir, "fetch.log"), filepath.Join(dir, "notify.log"), stepCacheNotes)

	const (
		messages = "      - Review the notes and summarize them.\n"
		assert   = "    assert:\n      files: [report/summary.md]\n"
		nudge    = "      nudge: true\n"
	)

	rewritePipeline(t, path, messages, messages+assert+nudge)
	mustRun(t, "test", "--workspace-root", root, path)
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != len(script) {
		t.Fatalf("provider requests after an unchanged rerun = %d, want %d — the step was not cached, so nothing below proves anything", got, len(script))
	}

	rewritePipeline(t, path, assert+nudge, assert)
	mustRun(t, "test", "--workspace-root", root, path)

	if got := fake.requestCount(); got != 2*len(script) {
		t.Errorf("provider requests after the nudge was removed = %d, want %d — the cached step was served as if it had run unaided", got, 2*len(script))
	}
}

// The cache is filed per pipeline. Two pipelines on one machine are two authors' work, and an entry one wrote — over its own caches:, under its own secrets — is not the other's to reuse; the same step rerun in its own pipeline still is.
func TestStepCacheIsNotSharedBetweenPipelines(t *testing.T) {
	t.Parallel()

	dir, root := t.TempDir(), t.TempDir()
	ranLog := filepath.Join(dir, "ran.log")

	pipeline := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: compile
    outputs: [out]
    run: |
      echo ran >> %s
      echo built > out/bin
`, ranLog)

	first := filepath.Join(dir, "first.yml")
	second := filepath.Join(dir, "second.yml")
	writePipelineFile(t, first, pipeline)
	writePipelineFile(t, second, pipeline)

	// One state database, as a daemon's two pipelines share one.
	db := filepath.Join(dir, "state.db")

	mustRun(t, "run", "--db", db, "--workspace-root", root, first)
	mustRun(t, "run", "--db", db, "--workspace-root", root, first)
	assertLineCount(t, ranLog, 1)

	mustRun(t, "run", "--db", db, "--workspace-root", root, second)
	assertLineCount(t, ranLog, 2)
}
