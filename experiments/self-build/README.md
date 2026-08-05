# self-build: dogfooding `steps` to build its own roadmap

An experiment: use `steps` itself to walk its own roadmap, one story at a
time. [pipeline-agent.yml](pipeline-agent.yml) is the whole thing — no
generator, no per-story duplication.

The stories are [GitHub issues](https://github.com/jtarchie/steps/issues)
labeled `story`, one per roadmap feature. The pipeline's `stories` resource
lists the open ones, and each run plans, implements, gates, reviews, and (only
on a passing review) opens a PR whose body says `Closes #N` — so merging
retires the story and it stops appearing on the next check. Earlier revisions
kept the stories as markdown files in the repo and chose between them by
hand-editing a glob; both are gone, and `source.limit` is the selector now.

> **⚠️ Parts of this document describe earlier revisions.** Two changes have
> outrun it. First, the repository became an ordinary `get: repo` input
> artifact, which deleted the custom shell tools described below
> (`story_brief`, `read_repo_file`, `repo_shell`, `stories/repo`, …) — the
> pipeline's own header comment is current on that. Second, there used to be a
> second variant, `pipeline.yml`, driving `claude -p` instead of `agent:`
> steps; it has been deleted, so everything about `plan`/`execute`/`critique`
> as `task:` steps, `--allowedTools`, `--permission-mode`, `source.repo`, and
> the streaming-output notes is history, not description. The accurate parts
> are this intro, "`stories` resource" under How it's wired, "Before running
> this for real", and "Running it".

The agents are `steps`' own `agent:` step type rather than a shelled-out
`claude` CLI, which is what lets the experiment run against a local or cheap
model (LM Studio/Ollama, OpenRouter). See the pipeline's header comment for
the full rationale; the short version:

- **No free CLAUDE.md pickup.** `claude` reads it from the checkout's cwd
  automatically; an `agent:` step doesn't, so the `stories` resource's `in:`
  materializes a copy of CLAUDE.md (and the story doc) into the artifact
  instead, so built-in `read_file` (100,000-byte cap) reads it whole in one
  call rather than a shell round-trip every story needs regardless. This is
  how every persona/prompt in `pipeline-agent.yml` gets the same standing
  rules a real code review found the *first* self-build run needed and
  didn't get: a behavior added to one of several similar code paths
  (get/task/put/agent step handling) but not the others.
- **No `dir:` that reaches the real checkout.** `read_file`/`list_dir`/
  `write_file` are confined to the agent step's own ephemeral workspace
  (symlink-escape checked), unlike a task's `run:`, which can `cd` anywhere.
  Each agent below is granted a small set of custom, narrowly-named tools
  instead of one do-everything shell: `story_brief` (the bootstrap, one call,
  no arguments), `read_repo_file`/`read_repo_lines`/`list_repo_dir` (reading,
  always `cd "$(cat stories/repo)"` first), and a role-specific write tool
  whose output path is derived from `stories/num` in shell — never a model
  argument. Only `coder` gets a general-purpose `repo_shell` (git, tests,
  lint, build, edits) and `write_repo_file` (arbitrary path, content as an
  ordinary tool argument rather than a hand-escaped heredoc, since local
  models tend to mis-escape large multi-line heredocs assembled inside a
  JSON tool-call string); `planner` and `reviewer` each write exactly one
  file and have no shell at all. Code *search* (finding a symbol, every call
  site, a package's API) is pushed onto `gopls`' own MCP server
  (`gopls mcp`, run as a local stdio subprocess) instead of `grep -rn` — see
  `pipeline-agent.yml`'s own header comment for gopls' tool set and why it
  covers navigation but not file reading or general edits.
- **`verdicts:`/`to:`/`handoff:` replace the hand-rolled PASS/FAIL grep and
  critique-file re-read** the deleted `claude -p` variant needed, because
  those are agent-only mechanisms `claude -p` (a plain shell command from
  `steps`' point of view) can't participate in. The reviewer still writes a
  human-readable `experiments/self-build/critiques/<num>.md`, but routing is
  driven by the `verdict` tool call, not by grepping that file.

The "before running this for real" cautions below all apply (it makes real
commits and opens real PRs), plus one more: `planner` and `reviewer` are
structurally limited to writing one fixed file each (no `repo_shell`, no
general `write_repo_file` — see above), but `coder`'s `repo_shell` is exactly
as unconfined as `claude`'s bare `Bash` would be, the same
"no path/command confinement by design" trade `internal/shell.HostRunner`
itself documents — nothing in `steps`' tool-grant system can restrict what a
granted `repo_shell`/`write_repo_file` call actually does, the way `claude`'s
`--allowedTools "Bash(git diff*)"` scopes `critique` to read-only commands.
The boundary there is `coder`'s persona instructions, not a hard guarantee.

## How it's wired

- **`stories` resource** (`resource_types:`/`resources:`): in
  `pipeline-agent.yml`, `check:` is `gh issue list --label story --state open`
  piped through a `--jq` filter that keeps issues titled `Story NNN: ...`,
  emits one version per issue (`{"num": "002", "issue": "8"}`), sorts by story
  number, and takes the first `source.limit` of them. `num` is parsed from the
  title rather than being the issue number, because it names the branch and
  the `plans/<num>.md` / `prs/<num>/` paths that already exist for earlier
  stories. `in:` then writes `num`, `issue`, `title`, and the issue body as
  `body.md` — the story text itself, where the file era only wrote a pointer
  into `repo/`.

  `source.limit` is the story selector. Every open story is a candidate, so
  `limit: 1` means "work the lowest-numbered open story", which is what the
  old glob did by being hand-narrowed to `001-*.md`. Raising it walks several
  per run, one after another.

  Closing an issue retires its story with no edit to the pipeline, and `out:`
  appends `Closes #N` to the PR body — so merging a self-build PR drops that
  story out of the next `check:` on its own.

  Both are a change from the file era, where `check:` globbed the story
  directory and `in:` wrote only a pointer into `repo/`.
- **`get: stories, version: every`** fans the rest of the plan out once per
  version — a feature built for "run the plan once per new resource
  version," repurposed here as "run the plan once per story." Each version's
  run gets an independent `max_visits:` budget (see
  [docs/control-flow.md](../../docs/control-flow.md)), which is what makes a
  per-story revise loop work without per-story step names.
- **`plan` → `execute` → `critique`**, three `task:` steps, written once.
  `critique` is a `to: {failure: execute}` / `max_visits: 4` loop: fail the
  gate, redo the execute step, up to 4 rounds before the job fails outward
  (which fires `self-build`'s own `on_failure`/`ensure`, if any are added).
- **`critique` has two gates**: an objective one first (`golangci-lint run &&
  go test ./... && go build -v` — cheap, deterministic, no model call needed
  to reject an obviously-broken attempt), then a qualitative one (`claude -p`
  reads the story doc and the working-tree diff, replies `PASS`/`FAIL`). Only
  a `PASS` commits.
- **How `execute` learns what `critique` found**: `task:` steps have no
  `handoff:`/`previous_run` mechanism — that's agent-only (see
  [docs/control-flow.md](../../docs/control-flow.md)) — so a redo round isn't
  automatically told why the last one failed. `execute` closes that gap
  itself: it reads `critique`'s own output file
  (`experiments/self-build/critiques/<num>.md`) back in, and if it starts
  with `FAIL`, appends its full text to the prompt as directed feedback
  before implementing again. A stale `PASS` file left over from an earlier,
  separate run of this pipeline is deliberately ignored (checked via `grep
  -q '^FAIL'`, not just file existence) so it's never mistaken for feedback.
  On the very first attempt, the file doesn't exist yet and `execute` gets no
  feedback section, exactly as before.
- **Why `claude` CLI instead of steps' own `agent:` step type**: this reuses
  Claude Code's existing coding-agent loop (file edits, git, iterative
  build/test/fix) rather than reimplementing one against `internal/agent`'s
  simpler tool set. It also means `CLAUDE.md` is picked up automatically by
  every invocation (each `claude -p` runs with the real checkout as its cwd
  — see "`source.repo`" below), so the package dependency graph and project
  layout it covers don't need to be restated in every prompt — the
  plan/execute/critique prompts point back to it explicitly, but the
  enforcement burden is only one file. No extra plumbing was needed for
  credentials either: `internal/shell.HostRunner` allowlists `HOME`, and
  `claude` CLI reads its stored auth from there.
- **`source.repo`**: `steps` always runs task steps in a fresh, disposable
  temp workspace — never the directory `steps` was invoked from, with or
  without a `workspace:` block — and `version: every` gives each *story* its
  own such workspace, with no memory of what an earlier story committed
  (confirmed by actually running this: the first real attempt showed `claude`
  a directory containing only the `stories/` artifact, no `CLAUDE.md`, no
  `internal/`). `plan`/`execute`/`critique` each read `stories/repo` (written
  by the resource's `in:` from `source.repo`) and `cd` into it as their first
  action, so everything after that — reading the story doc, editing real
  files, `git commit` — happens against the actual, persistent checkout, not
  the ephemeral one. This is *why* stories can build on each other at all:
  it's not steps's workspace machinery doing that, it's this pipeline
  deliberately stepping outside it.
- **Model**: all three steps pin `--model haiku`, a deliberate cost/time
  tradeoff over the default — 14 stories × up to 4 rounds × 3 calls adds up
  fast (see "Cost and time" below), and Haiku is the cheapest, fastest model
  in the family. The real risk is on `execute`: several of these stories
  (`in_parallel`, `passed:`, cross-job locking) are genuine Go concurrency
  work, and this is a smaller/faster model attempting it, not the strongest
  one available. Worth revisiting per-step if `execute`'s pass rate turns out
  low in practice — nothing here requires all three steps to share one
  model.
- **Live output** took fixing *two* independent layers of buffering, found
  by actually running this pipeline (minutes of silence, then everything at
  once) and diagnosing with a byte-level timestamp reader:
  1. **`claude` itself.** Its default `--output-format text` buffers the
     *entire* turn and emits nothing until the whole response is ready. Each
     call adds `--output-format stream-json --include-partial-messages
     --verbose` (the last is required alongside `stream-json` under
     `--print`, or `claude` exits 1) piped through a small Ruby filter that
     prints each `text_delta` as it arrives. `plan`/`execute` use the simple
     form (print every delta live; nothing downstream reads their output, so
     showing intermediate tool-narration text alongside the real work is
     fine, even useful). `critique` needs a stricter, two-stream version:
     intermediate narration streams as `text_delta` identically to a final
     answer (verified empirically), so its filter prints everything live to
     stderr but tracks `message_start`/`message_stop` boundaries to send only
     the *last* message's text to stdout — which is what `> "$out"` captures
     and `grep -q '^PASS'` checks.
  2. **`steps`'s own prefixer.** Even with the deltas arriving incrementally,
     nothing streamed — `internal/shell`'s `prefixedWriter`
     ([prefix.go](../../internal/shell/prefix.go)) originally buffered each
     line to its terminating `\n` before prefixing it, and the Ruby filters
     `print` newline-less token chunks, so everything piled up until a
     paragraph break. It was rewritten to prefix at line *starts* and forward
     every byte immediately (the way `gexec.PrefixedWriter` does), so
     sub-line content now streams as it arrives. This second fix is what
     actually made the `stream-json` deltas visible.

## Before running this for real

This has now completed once, on story 001, producing
[PR #1](https://github.com/jtarchie/steps/pull/1). Running it for real means
something qualitatively different from every other example in this repo:

- **It writes to the real GitHub repository.** Nothing local is at risk — the
  `repo` resource clones into the step's own ephemeral workspace, so the
  checkout you invoke from is never touched — but a passing review pushes a
  `self-build/story-NNN` branch to `origin` and opens a PR against `main`. To
  aim that somewhere safer, change the clone URL and `gh api` target in the
  `repo` resource type and the `--repo` in the `stories` resource type; there
  are four places and nothing cross-checks them.
- **`coder` holds an unrestricted `run_shell`.** That is full autonomy — no
  per-action approval — because the story loop has to edit code, add tests,
  and iteratively fix lint/test/build failures without a human in that inner
  loop. It is the one agent that trades away review; `planner` and `reviewer`
  hold no shell at all and can only write files inside their own workspace.
  `steps`' tool-grant system cannot confine what a granted `run_shell` does,
  so the boundary is the persona, not a guarantee.
- **PR #1 is the case for reading the result rather than trusting it.** It
  passed lint, tests, build, and the pipeline's own reviewer, and a second
  review still found ten confirmed defects. The harness weaknesses that
  allowed that are tracked as issues labeled
  [`self-build`](https://github.com/jtarchie/steps/labels/self-build).
- **Cost and time**: `coder` alone is budgeted at `timeout: 45m` per attempt
  with `attempts: 2`, and `build-check` can send it back up to 4 times while
  `reviewer` can send it back 3. One story is "implement a production feature
  with tests," not a quick query — expect hours unattended, not minutes, and
  multiply by `source.limit` if you raise it.
- **Ordering dependency between stories isn't enforced.** Story 002
  (`in_parallel`) is the keystone several later stories build on. Running in
  ascending order means a later story's `coder` should find earlier features
  already merged, but nothing *checks* that — and since `check:` lists open
  issues, an unmerged earlier story stays in the queue rather than blocking
  the ones that depend on it. That's on the prompts (which point back at
  CLAUDE.md and the issue's own cross-references) doing the right thing.

## Running it

Needs, on PATH and authenticated: `gh` (both resources are GitHub-backed —
`gh auth login` or `GH_TOKEN`), `gopls` (the MCP server the agents navigate
with), and credentials for whatever `agents: source.model` points at.

```
steps run experiments/self-build/pipeline-agent.yml --job self-build
```

No worktree setup and no local path to configure: the `repo` resource clones
`https://github.com/jtarchie/steps` into the step's own ephemeral workspace,
so nothing touches the checkout you invoke from. What it *does* touch is the
remote — a passing review pushes a `self-build/story-NNN` branch and opens a
real PR against `main`. Read the "Before running this for real" section above
before the first unattended run.

`source.limit: 1` already restricts a run to the lowest-numbered open story,
which is the right setting for a trial. To work a specific story instead, pin
the get step:

```yaml
- get: stories
  version: { num: "002", issue: "8" }
```

To widen it, raise `limit`; `version: every` then walks that many stories in
ascending order, one after another.

## Open design questions this sketch left for a real attempt

- Should `execute`'s `--allowedTools` be narrowed further (e.g. `Bash(go
  *)`/`Bash(git status)` instead of bare `Bash`) now that the exact commands
  it needs are known, rather than granting it unrestricted shell?
- Should a story that exhausts `max_visits` (4 failed rounds) do something
  other than fail the whole job — e.g. leave a `FAILED` marker and let the
  fan-out continue to the next story, so one stuck story doesn't block the
  other 13? That's exactly what approval steps
  ([#17](https://github.com/jtarchie/steps/issues/17)) and the watch circuit
  breaker ([#19](https://github.com/jtarchie/steps/issues/19)) would eventually
  give this pipeline if they existed yet — a fitting bootstrap problem.

- The harness's own weaknesses, surfaced by reviewing what it produced for
  story 001, are now tracked as issues labeled
  [`self-build`](https://github.com/jtarchie/steps/labels/self-build):
  the reviewer can't run anything it reviews
  ([#22](https://github.com/jtarchie/steps/issues/22)), the objective gate
  can't fail untested behavior
  ([#23](https://github.com/jtarchie/steps/issues/23)), the planner's sibling
  check misses enum-variant additions
  ([#24](https://github.com/jtarchie/steps/issues/24)), and a single reviewer
  verdict decides everything
  ([#25](https://github.com/jtarchie/steps/issues/25)).
