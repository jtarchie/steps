package main

import (
	"testing"
)

// docScenario is the invisible half of a doc example: everything a fenced
// block needs to run deterministically that would be noise on the rendered
// page. A block opts in by naming its scenario in the fence info
// (```yaml test=<id>); docs_test.go fails any agent-running block that
// doesn't, and any test= that names nothing here.
type docScenario struct {
	// fake builds the scripted provider every agent in the block is pointed
	// at. Reuse newFakeLLM (positional script) or newRoutedFakeLLM
	// (content-routed, for max_in_flight: concurrency) exactly as the e2e
	// tests do.
	fake func(t *testing.T) *fakeLLM

	// files are written into the pipeline's directory before the run — the
	// run_file:/prompt_file:/load_var: targets a doc example references.
	files map[string]string

	// vars are passed as --var flags: what an operator supplies for a
	// ((name)) the doc example deliberately leaves unresolved.
	vars map[string]string

	// check runs after a green `steps test`, for assertions the YAML itself
	// can't carry (which branch a verdict took, what a put received).
	check func(t *testing.T, dir string)
}

// scripted is the common case: a positional script of provider turns.
func scripted(turns ...turn) func(t *testing.T) *fakeLLM {
	return func(t *testing.T) *fakeLLM {
		t.Helper()

		return newFakeLLM(t, turns...)
	}
}

// docScenarios maps fence test= ids to their scaffolding. Keep ids
// page-scoped ("agents-review", not "review") so a rename never collides.
var docScenarios = map[string]docScenario{
	// The full worked pipeline (complete.md): read, write the report, approve
	// — which routes past escalate to the put.
	"complete-review": {
		fake: scripted(
			callsTool("read_file", map[string]any{"path": "repo/NOTES.txt"}),
			callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "Widgets seed from widgets.json on boot.\n"}),
			callsTool("verdict", map[string]any{"choice": "approve", "note": "accurate one-liner"}),
			says("Summary written and approved."),
		),
		check: func(t *testing.T, dir string) {
			t.Helper()

			nodes := storeNodes(t, dir+"/pipeline.yml")
			findNode(t, nodes, "put", "results")

			for _, node := range nodes {
				if node.Resource == "escalate" {
					t.Errorf("escalate ran, but the approve verdict should have routed past it")
				}
			}
		},
	},

	"internals-compaction": {
		fake: scripted(says("Done.")),
	},

	"internals-context-window": {
		fake: scripted(says("Reviewed.")),
	},

	"templating-vars": {
		vars: map[string]string{"repo_uri": "https://github.com/acme/app-staging"},
	},

	// The critic approves, so the route jumps forward to publish — escalate
	// must have recorded nothing.
	"control-verdicts": {
		fake: scripted(
			callsTool("verdict", map[string]any{"choice": "approve", "note": "reads fine"}),
			says("Approved the draft."),
		),
		check: func(t *testing.T, dir string) {
			t.Helper()

			nodes := storeNodes(t, dir+"/pipeline.yml")
			findNode(t, nodes, "task", "publish")

			for _, node := range nodes {
				if node.Resource == "escalate" {
					t.Errorf("escalate ran, but the approve verdict should have routed past it")
				}
			}
		},
	},

	// A bare-verdict classifier: the model must call the synthesized verdict
	// tool, and assert.verdict pins what it chose.
	"control-classifier": {
		fake: scripted(
			callsTool("verdict", map[string]any{"choice": "bug", "note": "crash on launch"}),
			says("Filed as a bug."),
		),
	},

	// The default read-only grant in action: the model reads the file it was
	// pointed at and answers.
	"agents-readonly": {
		fake: scripted(
			callsTool("read_file", map[string]any{"path": "notes/plan.txt"}),
			says("It says widgets ship on tuesday."),
		),
	},

	// dir: puts the model in repo/cmd, so its read_file path is main.go
	// rather than repo/cmd/main.go.
	"agents-dir": {
		fake: scripted(
			callsTool("read_file", map[string]any{"path": "main.go"}),
			says("main.go declares package main."),
		),
	},

	// The writer creates report/summary.md and the publish task proves the
	// artifact flowed.
	"agents-writer": {
		fake: scripted(
			callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "all clear\n"}),
			says("Wrote the findings."),
		),
	},

	// The model calls the required custom tool once; repo is pinned, so the
	// call authors only body.
	"agents-custom-tool": {
		fake: scripted(
			callsTool("post_review", map[string]any{"body": "looks correct"}),
			says("Posted the review."),
		),
	},

	// Parent delegates to the sub-agent tool; the child's own conversation is
	// the second request (serial, so positional scripting holds); the parent
	// then reports.
	"agents-subagent": {
		fake: scripted(
			callsTool("summarizer", map[string]any{"request": "summarize notes/log.txt"}),
			says("There was an outage; it is resolved."),
			says("Summary: there was an outage; it is resolved."),
		),
	},

	// Same shape as attempts-fix: the fixer writes the file the task demands.
	"agents-fix": {
		fake: scripted(
			callsTool("write_file", map[string]any{"path": "config.json", "content": "{}\n"}),
			says("Created config.json."),
		),
	},

	// run_file:/system_file:/prompt_file: all resolve from these files at
	// load; the conversation itself is a single reply.
	"agents-files": {
		files: map[string]string{
			"ci/unit.sh":          "echo unit tests pass\n",
			"ci/smoke.sh":         "echo smoke ok\n",
			"prompts/reviewer.md": "You review builds tersely.\n",
			"prompts/review.md":   "Review the build output.\n",
		},
		fake: scripted(says("Build looks fine.")),
	},

	"agents-task-file": {
		files: map[string]string{
			"ci/unit.yml": "run: echo from the shared task\n",
		},
	},

	// The prompt arrives from the fetched artifact at run time.
	"agents-prompt-artifact": {
		fake: scripted(says("The change is correct.")),
	},

	// The conventions file arrives as a synthetic read_file result, so the
	// model answers without calling any tool.
	"agents-context-paths": {
		fake: scripted(says("Always run go vet before committing.")),
	},

	// Two serial cells, one reply each — each cell's context_paths rendered
	// to its own file.
	"agents-across-context": {
		fake: scripted(
			says("api package reviewed."),
			says("storage package reviewed."),
		),
	},

	// reviewer decides (its note is required because editor demands it),
	// editor reads the note, and the gate task greps the delivered verdict
	// file.
	"agents-context-from": {
		fake: scripted(
			callsTool("verdict", map[string]any{"choice": "approve", "note": "small and safe"}),
			says("Approved."),
			says("Nothing to apply; the review approved."),
		),
	},

	"agents-budget": {
		fake: scripted(
			says("Announcement written.").spending(1200),
		),
	},

	// The primary (the fake) answers, so the declared fallback never fires.
	"agents-fallback": {
		fake: scripted(says("Announcement written.")),
	},

	// Three concurrent members vote approve; routed on content because
	// max_in_flight-style interleaving makes positional scripts racy. A
	// request whose last message is a tool result is a member being told its
	// vote landed; anything else is a member being asked to vote.
	"agents-ensemble": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
					return says("Voted.")
				}

				return callsTool("verdict", map[string]any{"choice": "approve", "note": "correct"})
			})
		},
		check: func(t *testing.T, dir string) {
			t.Helper()

			nodes := storeNodes(t, dir+"/pipeline.yml")
			findNode(t, nodes, "task", "publish")

			for _, node := range nodes {
				if node.Resource == "revise" {
					t.Errorf("revise ran, but the approve majority should have routed past it")
				}
			}
		},
	},

	// One transient 500, absorbed by the agent default of attempts: 3 — the
	// retry re-sends the failing request, and the conversation carries on.
	"attempts-transient": {
		fake: scripted(
			failsWith(500),
			says("Release looks good."),
		),
	},

	// The task fails because patched.txt is missing; the fix agent writes it
	// into the task's own working directory, and the re-run passes.
	"attempts-fix": {
		fake: scripted(
			callsTool("write_file", map[string]any{"path": "patched.txt", "content": "ok\n"}),
			says("Created the missing file."),
		),
	},

	"attempts-agent-timeout": {
		fake: scripted(
			says("No safety issues found."),
		),
	},

	// The first cell reports spending double the block's budget, so cells two
	// and three are never admitted — the script has exactly one turn, and a
	// second request would fail the test by exhausting it.
	"control-across-budget": {
		fake: scripted(
			says("api boundaries look fine").spending(2000),
		),
	},
}
