# steps

A CLI that runs Concourse-style YAML pipelines, with LLM agent steps built in.

Pipelines fetch resources, run tasks, and can hand off to an `agent` step that talks to an LLM with tool-calling support (`read_file`, `list_dir`, `run_shell`, or your own custom tools). State is cached in SQLite, so unchanged steps are skipped on the next run.

## Install

```bash
brew tap jtarchie/steps https://github.com/jtarchie/steps
brew install steps
```

macOS only — the tap ships a cask, and Homebrew on Linux doesn't install casks. On Linux, grab the tarball for your arch from [releases](https://github.com/jtarchie/steps/releases).

## Build

```bash
go build -v
```

Requires Go 1.26+.

## Quick start

```yaml
# pipeline.yml
jobs:
- name: hello
  plan:
  - task: greet
    run: echo "hello from steps"
```

```bash
./steps run pipeline.yml
```

`--job` is only needed when the pipeline has more than one job.

Fetching something real needs a resource. `git` is built in, so this still needs no `resource_types:` block:

```yaml
resources:
- name: repo
  type: git
  source:
    uri: https://github.com/jtarchie/ci.git
    branch: main

jobs:
- name: build
  plan:
  - get: repo
  - task: compile
    inputs: [repo]     # a step sees only the artifacts it declares
    run: cd repo && go build ./...
```

See [`docs/resources.md`](docs/resources.md) for other resource types and the `check`/`in`/`out` contract.

## Commands

| Command | What it does |
|---|---|
| `steps run <pipeline>` | Run one job once. |
| `steps validate <pipeline>` | Check the file for errors without running anything. |
| `steps plan <pipeline>` | Show which steps a run would execute and which are cached. |
| `steps runs <pipeline>` | Show what past runs recorded (`--steps`, `--queue`). |
| `steps watch <pipeline>` | Poll `trigger: true` resources and run affected jobs. |
| `steps test <pipeline>` | Run every job and check `assert:` directives. |
| `steps web <pipeline>...` | Serve a local browser UI, and poll trigger resources while it serves ([docs](docs/web.md)). |
| `steps mcp list\|tools\|login` | List, inspect, or authorize `mcp_servers:` entries. |

Exit codes: `0` success, `1` a step failed, `2` the pipeline could not be run (config or infrastructure), `130` interrupted.

## Learn more

- [`docs/`](docs/README.md) — indexed reference: start with [resources](docs/resources.md), then [control flow](docs/control-flow.md) or [agents](docs/agents.md).
- [`steps web`](docs/web.md) — run transcripts with cached steps folded, agent conversations expanded, the job dependency graph, and live runs streaming as they happen.
- Every YAML example in `docs/` is a complete, minimal pipeline the test suite extracts and executes — copy one out and run it. Read them with `steps docs [page]` in a terminal or at `/docs` in the web UI. Agent examples name `openrouter/qwen/qwen3.7-flash` (cheap, tool-calling; needs `OPENROUTER_API_KEY`) — swap the model for yours, including a local server that needs no key (`lmstudio/your-model`).
- [`examples/pr-review.yml`](examples/pr-review.yml) — the capstone: an adaptive PR review whose matrix width a planner step decides mid-run, reviewers fanned out concurrently, findings collected and synthesized, a human approval before anything posts. Needs a live model and an authenticated `gh`: `PR_REPO=owner/name steps run examples/pr-review.yml --job review`.
- [`CLAUDE.md`](CLAUDE.md) — architecture, build constraints, and contribution notes for anyone (human or agent) changing this codebase.
