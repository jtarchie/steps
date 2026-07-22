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
./steps run pipeline.yml --job hello
```

## Commands

- `steps run <pipeline.yml> --job <name>` — run one job once.
- `steps watch <pipeline.yml>` — poll `trigger: true` resources and automatically run affected jobs.
- `steps test <pipeline.yml>` — run every job and check any `assert:` directives (useful for self-verifying, modelless pipeline fixtures).

## Learn more

- [`examples/`](examples/) — runnable example pipelines, one per feature area (control flow, agents, infra, workspace isolation).
- [`docs/`](docs/) — feature-by-feature reference (agent tool semantics, hooks/conditionals/routing, containers, triggers, workspace isolation, templating).
- [`CLAUDE.md`](CLAUDE.md) — architecture, build constraints, and contribution notes for anyone (human or agent) changing this codebase.
