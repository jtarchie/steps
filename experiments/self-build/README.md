# self-build: dogfooding `steps` to build its own roadmap

An experiment: use `steps` itself to walk [../stories/](../stories/) in
order, using the `claude` CLI as the plan/execute/critique worker for each
story. [pipeline.yml](pipeline.yml) is the whole thing — no generator, no
per-story duplication.

There is a second variant, [pipeline-agent.yml](pipeline-agent.yml), that
does the same walk using `steps`' own `agent:` step type instead of the
`claude` CLI — for trying this against a local model (LM Studio/Ollama)
rather than Claude. See its own header comment for the full rationale; the
short version:

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
  critique-file re-read** `pipeline.yml`'s `critique` task needed, because
  those are agent-only mechanisms `claude -p` (a plain shell command from
  `steps`' point of view) can't participate in. The reviewer still writes a
  human-readable `experiments/self-build/critiques/<num>.md`, but routing is
  driven by the `verdict` tool call, not by grepping that file.

Same "before running this for real" cautions apply as `pipeline.yml`
(`source.repo` needs to point at a dedicated worktree, it makes up to 14 real
commits, etc. — see below) plus one more: `planner` and `reviewer` are
structurally limited to writing one fixed file each (no `repo_shell`, no
general `write_repo_file` — see above), but `coder`'s `repo_shell` is exactly
as unconfined as `claude`'s bare `Bash` would be, the same
"no path/command confinement by design" trade `internal/shell.HostRunner`
itself documents — nothing in `steps`' tool-grant system can restrict what a
granted `repo_shell`/`write_repo_file` call actually does, the way `claude`'s
`--allowedTools "Bash(git diff*)"` scopes `critique` to read-only commands.
The boundary there is `coder`'s persona instructions, not a hard guarantee.

## How it's wired

- **`stories` resource** (`resource_types:`/`resources:` in pipeline.yml):
  `check:` globs `docs/proposals/stories/000-*.md .. 013-*.md` and emits one
  version per file (`{"num": "000", "file": "000-...md"}`, ...), via a small
  Ruby one-liner (`Dir.glob(...).sort.map{...}.to_json`) rather than
  hand-rolled `printf`/comma-tracking — the one part of this resource type
  that's real data manipulation rather than shelling out to another CLI.
  `.source.dir` is passed as a `ruby -e '...' -- <arg>` process argument
  (`shellquote`'s actual job), not interpolated into the Ruby source, so it
  needs no Ruby-side escaping.
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

This has now been *attempted* once (interrupted early, on purpose — see
"`source.repo`" above for what that run caught) but never completed. Running
it for real means something qualitatively different from every other example
in this repo:

- **`source.repo` currently points at `/Users/jtarchie/workspace/steps` —
  the primary checkout, not a worktree.** Change this to a dedicated
  worktree's absolute path (see "Running it" below) before a real run.
  Nothing in the pipeline itself enforces isolation; the field is a plain
  string, easy to forget to change back.
- **`execute` runs `claude` with `--permission-mode bypassPermissions`.**
  That's full autonomy — no per-action approval — because the story loop
  needs to edit code, add tests, and iteratively fix lint/test/build failures
  without a human in that inner loop. This is the one step in the whole
  pipeline that trades away human review; every other step (`plan`,
  `critique`'s qualitative check) uses a narrower `--allowedTools` list or
  the default permission mode.
- **It will make up to 14 real commits** (one per story, only on a critique
  `PASS`) with real code changes, each potentially preceded by up to 3 redo
  rounds, directly onto whatever branch `source.repo` has checked out.
- **Cost and time**: 14 stories × up to 3 `claude` calls per round × up to 4
  rounds is a real amount of model usage and wall-clock time in the worst
  case — `execute` in particular is "implement a production feature with
  tests," not a quick query. Expect this to run for hours unattended, not
  minutes.
- **Ordering dependency between stories isn't enforced.** Story 002
  (`in_parallel`) is the keystone several later stories build on; running
  the full sequence in order means later stories' `execute` should find
  earlier features already merged, but nothing here *checks* that a later
  story's implementation actually leans on an earlier one — that's on the
  plan/execute prompts (which do point back at CLAUDE.md and the story's own
  cross-references) doing the right thing, not a mechanical guarantee.

## Running it

Whole sequence, in a dedicated worktree (recommended — set up the worktree
*and* point `source.repo` at it, both are required):

```
git worktree add ../steps-self-build -b self-build
```

Edit `pipeline.yml`'s `resources: - name: stories - source: repo:` to the
worktree's absolute path, then, from either directory (task steps `cd` into
`source.repo` regardless of where `steps` itself was invoked from):

```
steps run experiments/self-build/pipeline.yml --job self-build
```

One story only, for a first trial run: temporarily restrict the `stories`
resource's `check:` to just that story's file (or pin the get step with
`version: {num: "000"}` instead of `every`) before trusting the full
14-story unattended run.

## Open design questions this sketch left for a real attempt

- Should `execute`'s `--allowedTools` be narrowed further (e.g. `Bash(go
  *)`/`Bash(git status)` instead of bare `Bash`) now that the exact commands
  it needs are known, rather than granting it unrestricted shell?
- Should a story that exhausts `max_visits` (4 failed rounds) do something
  other than fail the whole job — e.g. leave a `FAILED` marker and let the
  fan-out continue to the next story, so one stuck story doesn't block the
  other 13? That's exactly what [011-approval-steps](../stories/011-approval-steps.md)
  and [013-watch-circuit-breaker](../stories/013-watch-circuit-breaker.md)
  would eventually give this pipeline if they existed yet — a fitting
  bootstrap problem.
