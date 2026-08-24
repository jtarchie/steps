package main

import (
	"slices"
	"strings"
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

	// fallbackFake, when set, builds a SECOND scripted provider and points
	// every agent's fallback: [0].source at it — for a doc example that needs
	// its fallback to actually be reachable (and, for a mid-run scenario,
	// actually fire) rather than the common case of a declared-but-never-
	// dialed backup.
	fallbackFake func(t *testing.T) *fakeLLM

	// files are written into the pipeline's directory before the run — the
	// run_file:/message_files:/load_var: targets a doc example references.
	files map[string]string

	// vars are passed as --var flags: what an operator supplies for a
	// ((name)) the doc example deliberately leaves unresolved.
	vars map[string]string

	// workers are passed as --worker flags: the tag-to-machine mapping an
	// operator supplies for a step's tags:. Built per test, like fake, so a
	// scenario can point at something it started; a scenario that only needs
	// placement to HAPPEN names local:, which runs the step through a shim on
	// this machine and needs no network.
	workers map[string]string

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
	// A placed step, run through a shim in a child process on this machine:
	// no network, no worker, no credentials, and the whole transport — frames,
	// tree round trip, exit codes — exercised for real rather than stubbed.
	// Two user turns, so the provider answers twice: the second script entry is
	// what the model says once it has been asked the follow-up.
	"agents-two-messages": {
		fake: scripted(
			says("Safe to ship."),
			says("It turns on parser.go line 42."),
		),
	},
	"infra-worker": {
		workers: map[string]string{"gpu": "local:"},
	},

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

	// The default read-only grant, pinned on delivery: the router answers
	// only after read_file's RESULT carries the fixture file's text. A
	// positional script would replay the answer even if the grant broke —
	// assert.tool_calls records what the model REQUESTED, success or not —
	// so a failed read has to starve the answer here to red out the fixture.
	"agents-readonly": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				switch {
				case req.toolResultContains("widgets ship on tuesday"):
					return says("It says widgets ship on tuesday.")
				case len(req.toolResults()) > 0:
					return says("The file could not be read.")
				default:
					return callsTool("read_file", map[string]any{"path": "notes/plan.txt"})
				}
			})
		},
	},

	// dir: puts the model in repo/cmd, so its read_file path is main.go
	// rather than repo/cmd/main.go — and the router answers only after that
	// dir:-relative path actually delivered the file's content.
	"agents-dir": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				switch {
				case req.toolResultContains("package main"):
					return says("main.go declares package main.")
				case len(req.toolResults()) > 0:
					return says("The file could not be read.")
				default:
					return callsTool("read_file", map[string]any{"path": "main.go"})
				}
			})
		},
	},

	// The writer creates report/summary.md and the publish task proves the
	// artifact flowed.
	"agents-writer": {
		fake: scripted(
			callsTool("write_file", map[string]any{"path": "report/summary.md", "content": "all clear\n"}),
			says("Wrote the findings."),
		),
	},

	// The allow: fence, proven by its refusal: the fetch targets a host
	// outside the list, the refusal comes back as tool-result data (before
	// any connection — the example needs no network), and the model records
	// it. Routed rather than positional so a broken fence — the fetch
	// somehow succeeding, or erroring differently — starves the write.
	"agents-web-fetch": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				switch {
				case req.historyCalled("write_file"):
					return says("Recorded the refusal.")
				case req.toolResultContains("allow: list"):
					return callsTool("write_file", map[string]any{
						"path":    "notes/status.md",
						"content": "fetch refused: issues.example is outside this pipeline's allow list\n",
					})
				default:
					return callsTool("web_fetch", map[string]any{"url": "https://issues.example/open"})
				}
			})
		},
	},

	// required: enforced on the wire: the model first tries to stop, the
	// loop forces post_review via tool_choice (the router calls it only when
	// forced), and the answer comes after the tool result lands. repo is
	// pinned, so the call authors only body.
	"agents-custom-tool": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				switch {
				case req.forcedTool() == "post_review":
					return callsTool("post_review", map[string]any{"body": "looks correct"})
				case len(req.toolResults()) > 0:
					return says("Posted the review.")
				default:
					return says("I reviewed the change; nothing more to do.")
				}
			})
		},
	},

	// Parent delegates to the sub-agent tool. The child is the request NOT
	// offered a `summarizer` tool (only the parent's grant includes it), and
	// the parent reports only after the child's answer arrives as its tool
	// result — so a broken dispatch (the parent consuming the child's turn,
	// or the child's answer never fed back) starves the router instead of
	// passing on a lucky stdout match.
	"agents-subagent": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if !slices.Contains(req.toolNames(), "summarizer") {
					return says("There was an outage; it is resolved.")
				}

				switch {
				case req.toolResultContains("outage; it is resolved"):
					return says("Summary: there was an outage; it is resolved.")
				case len(req.toolResults()) > 0:
					return says("The summarizer returned nothing useful.")
				default:
					return callsTool("summarizer", map[string]any{"request": "summarize notes/log.txt"})
				}
			})
		},
	},

	// Same shape as attempts-fix: the fixer writes the file the task demands.
	"agents-fix": {
		fake: scripted(
			callsTool("write_file", map[string]any{"path": "config.json", "content": "{}\n"}),
			says("Created config.json."),
		),
	},

	// run_file:/system_file:/message_files: all resolve from these files at
	// load — and the router answers only when the persona text is in the
	// system message AND the prompt text is in the user message, so the
	// files' CONTENT reaching the conversation is what is pinned, not just
	// that the includes loaded.
	// The nudge, end to end in a doc example: the model answers in prose,
	// steps tells it the file it owes is missing, and it writes it. Routed on
	// the nudge's own text rather than positionally, so an implementation
	// that stopped telling the model starves the write instead of quietly
	// scripting one.
	"agents-delivers-files": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				switch {
				case req.historyCalled("write_file"):
					return says("Written.")
				case req.userMessageContains("must leave behind"):
					return callsTool("write_file", map[string]any{
						"path":    "answer/reply.md",
						"content": "The catalog is seeded from widgets.json.\n",
					})
				default:
					return says("The catalog is seeded from widgets.json.")
				}
			})
		},
	},

	"agents-files": {
		files: map[string]string{
			"ci/unit.sh":          "echo unit tests pass\n",
			"ci/smoke.sh":         "echo smoke ok\n",
			"prompts/reviewer.md": "You review builds tersely.\n",
			"prompts/review.md":   "Review the build output.\n",
		},
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if strings.Contains(req.systemMessage(), "tersely") && req.userMessageContains("Review the build output.") {
					return says("Build looks fine.")
				}

				return says("The persona or prompt never arrived.")
			})
		},
	},

	// An empty script IS the assertion: both jobs' tasks pass, so the fix
	// agent is never constructed and the provider is never called. Any
	// request at all fails this fixture.
	"agents-tasks-reuse": {
		fake: scripted(),
	},

	"agents-task-file": {
		files: map[string]string{
			"ci/unit.yml":     "run: echo from the shared task\n",
			"ci/reviewer.yml": "source: { model: openrouter/qwen/qwen3.7-flash }\n",
		},
		fake: scripted(says("Nothing to flag in this build.")),
	},

	// The prompt arrives from the fetched artifact at run time: the router
	// answers only when REVIEW.md's text is the user message, so what is
	// pinned is content delivery, not just that the file existed.
	"agents-prompt-artifact": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if req.userMessageContains("Review this change for correctness.") {
					return says("The change is correct.")
				}

				return says("No review instructions arrived.")
			})
		},
	},

	// The conventions file is injected as a synthetic read_file result at
	// turn zero — so request 1 must ALREADY carry its text, before the model
	// has called anything. The router answers from nothing else; a broken
	// injection starves it and the stdout assert goes red.
	"agents-context-paths": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if req.toolResultContains("always run go vet") {
					return says("Always run go vet before committing.")
				}

				return says("The conventions never arrived.")
			})
		},
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

	// The two agents are told apart by drafter's temperature dial reaching
	// the wire — titler sets none, and an unset dial is never sent. A dial
	// that stopped being sent (or a defaults.model that stopped filling
	// titler in) flips or drops an answer and a stdout assert goes red.
	"agents-dials": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if strings.Contains(req.Raw, `"temperature"`) {
					return says("Drafted the release note.")
				}

				return says("Titled the release note.")
			})
		},
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

	// The primary completes a real tool turn and THEN dies; attempts: 1
	// leaves no room to retry, so the cascade fires — and the fallback
	// (fallbackFake, a SECOND provider — see injectFakeFallback) answers
	// only when that completed turn's traffic arrives in its own first
	// request. "Resumes the same conversation" is the thing tested here,
	// not just claimed: a fresh-history resume starves the router.
	"agents-fallback-midrun": {
		fake: scripted(
			callsTool("read_file", map[string]any{"path": "NOTES.txt"}),
			failsWith(500),
		),
		fallbackFake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if req.historyCalled("read_file") && len(req.toolResults()) > 0 {
					return says("Announcement written via the fallback.")
				}

				return failsWith(500)
			})
		},
	},

	// A genuine 2–1 split: the style reviewer rejects, the other two approve
	// — the one tally where majority, unanimous (would fail the block), and
	// any (reject is listed first, would route to revise) all disagree, so
	// decide: majority is the rule actually exercised. Members are told
	// apart by their differentiated prompts; routed on content because they
	// run concurrently. A request whose last message is a tool result is a
	// member being told its vote landed.
	"agents-ensemble": {
		fake: func(t *testing.T) *fakeLLM {
			t.Helper()

			return newRoutedFakeLLM(t, func(req capturedRequest) turn {
				if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Role == "tool" {
					return says("Voted.")
				}

				if req.userMessageContains("style") {
					return callsTool("verdict", map[string]any{"choice": "reject", "note": "naming is inconsistent"})
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

	// A provider that only ever 500s: with attempts: 1 the single request
	// fails, the step classifies errored, and on_error is what fires — the
	// doc corpus's one genuine errored trigger now that an expired timeout:
	// classifies as failed.
	"attempts-provider-error": {
		fake: scripted(
			failsWith(500),
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

	// A tool that hangs, bounded by its own timeout: rather than the step's.
	// The model calls it, is handed the expiry as ordinary tool-result data
	// on its next turn, and answers on the strength of what it could not
	// learn — which is the whole point of reporting the deadline instead of
	// killing the step.
	"internals-tool-timeout": {
		fake: scripted(
			callsTool("await_rollout", map[string]any{"service": "widgetd"}),
			says("The log tailer timed out, so the rollout status is unknown."),
		),
	},

	"attempts-agent-timeout": {
		fake: scripted(
			says("No safety issues found."),
		),
	},

	// Two steps of one agent, each answering once: what the block is showing
	// is that neither had to restate the entry's timeout:/attempts:/max_turns:.
	"attempts-agent-dials": {
		fake: scripted(
			says("Looks correct."),
			says("No injection paths."),
		),
	},

	// The uncapped agent answers on its first turn. The dials being 0 is the
	// point of the block; that a conversation with no ceiling still ENDS when
	// the model is done is the point of running it.
	"attempts-no-limits": {
		fake: scripted(
			says("migration complete"),
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
