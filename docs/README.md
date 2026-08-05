# Documentation

Read the page for what you're doing. Nothing here needs to be read in order, except that [resources.md](resources.md) is the one most people need second, right after the quick start.

## Writing pipelines

| Page | What it covers | Length |
|---|---|---|
| [resources.md](resources.md) | Resources and resource types: the built-in `git`, the `check`/`in`/`out` contract, `version:`, `trigger:` | short |
| [control-flow.md](control-flow.md) | `when:` guards, hooks, `to:`/`max_visits:` routing, verdicts, handoff, `assert:` | medium |
| [agents.md](agents.md) | Agent steps: tools, prompts, verdicts, `fix:`, handoff notes, sub-agents | long |
| [attempts-timeout.md](attempts-timeout.md) | `attempts:` and `timeout:`, and how they interact | short |
| [workspace.md](workspace.md) | `inputs:`/`outputs:` and opt-in per-step isolation | short |
| [infra.md](infra.md) | Cross-job triggers (`steps watch`) and containerized execution (`image:`) | short |
| [templating.md](templating.md) | `{{ }}` in resource commands and custom tools, and `shellquote` | short |
| [mcp.md](mcp.md) | MCP servers as tool sources and as resource-type backends | medium |

## Reference

| Page | What it covers |
|---|---|
| [agents-internals.md](agents-internals.md) | How agent steps work underneath: transport, tool-call repair, compaction, caching |
| [conformance.md](conformance.md) | Which Concourse behaviors steps matches, which it doesn't, and which are verified |

## Commands

```
steps run <pipeline>        run one job
steps watch <pipeline>      poll trigger: true resources, run affected jobs
steps test <pipeline>       run every job and check assert: directives
steps validate <pipeline>   check the file, and that this machine can run it
steps plan <pipeline>       show what a run would execute vs skip
steps runs <pipeline>       show what past runs recorded
steps mcp tools|login       inspect or authorize mcp_servers: entries
```

Two of these answer most "why is it doing that?" questions: `steps plan` explains what the cache would skip, and `steps runs --steps` shows what previous runs actually did.

`steps validate` answers a third: *will this run at all?* It checks the file — syntax, references, field placement — and then the things the file depends on that live outside it:

- every model name resolves to a known provider (a typo like `opencoder/` for `opencode/` is a load error, not a failed run)
- every `api_key_env:` the pipeline names is actually set
- every stdio `mcp_servers:` command is actually on `PATH`

It reports all of them at once, because finding them one run at a time is the problem:

```
$ steps validate pipeline.yml
steps: error: pipeline.yml cannot run here:
  agent "coder"  $OPENROUTER_API_KEY is not set (source.api_key_env)
  mcp "gopls"    command "gopls" not found on PATH
```

Add `--syntax-only` to check the file alone. That is the right flag for a pre-commit hook or a CI lint of a pipeline that build has no intention of running — it should not need that pipeline's production credentials on hand.

## Editor support

[`steps.schema.json`](../steps.schema.json) is a JSON Schema for the pipeline format. Point your editor at it with a modeline on the first line of a pipeline, as every example does:

```yaml
# yaml-language-server: $schema=./steps.schema.json
```

That gives completion and inline errors while you type. `steps validate` remains the authority — it checks rules a schema can't express, like whether a `to:` target exists in the same segment — but the schema catches misspelled keys at the keystroke rather than the run.

## Examples

[`examples/`](../examples/) holds runnable, self-contained pipelines, several of which verify themselves under `steps test`. Its [`invalid/`](../examples/invalid/) subdirectory is the inverse: pipelines that must be **rejected** at load, each naming the error it has to produce. That's where a rule like "`trigger:` is only valid on `get` steps" gets a file you can read, rather than living only as an error-substring assertion in a Go test.
