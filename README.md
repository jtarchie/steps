# steps

A CLI that runs Concourse-style YAML pipelines, with LLM agent steps built in.

Pipelines fetch resources, run tasks, and can hand off to an `agent` step that talks to an LLM with tool-calling support (`read_file`, `list_dir`, `run_shell`, or your own custom tools). State is cached in SQLite, so unchanged steps are skipped on the next run.

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
| `steps mcp tools\|login` | Inspect or authorize `mcp_servers:` entries. |

Exit codes: `0` success, `1` a step failed, `2` the pipeline could not be run (config or infrastructure), `130` interrupted.

## Learn more

- [`docs/`](docs/README.md) — indexed reference: start with [resources](docs/resources.md), then [control flow](docs/control-flow.md) or [agents](docs/agents.md).
- [`examples/`](examples/) — runnable pipelines, one per feature area. Agent examples ship pointed at `openrouter/qwen/qwen3.7-flash` (cheap, tool-calling) and need `OPENROUTER_API_KEY`. `$STEPS_MODEL` overrides that without editing a file — including onto a local server, which costs nothing and needs no key: `STEPS_MODEL=lmstudio/your-model steps run examples/agents.yml --job review`.
- [`CLAUDE.md`](CLAUDE.md) — architecture, build constraints, and contribution notes for anyone (human or agent) changing this codebase.
