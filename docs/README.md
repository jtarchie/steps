# Documentation

Read the page for what you're doing. Nothing here needs to be read in order, except that [resources.md](resources.md) is the one most people need second, right after the quick start.

**Every YAML example in these docs is a complete pipeline, extracted and executed by the test suite** (`docs_test.go`). An example that needs docker, btrfs, the network, a CLI, or real credentials says which one; everything else runs exactly as shown, and a doc example that stops working fails the build. Read them via `steps docs` in a terminal, `/docs` in the [web UI](web.md), or the files themselves.

**The `assert:` blocks you see in them are load-bearing, not decoration.** Every executed example verifies its own behavior — which steps ran in what order, what a command printed, what a model decided, what a step wrote — so the examples are regression tests for the features they document, and the claims in the prose beside them are checked rather than asserted by an author. A separate mutation suite proves those assertions can actually fail: it rewrites each one to something the pipeline does not satisfy and requires the build to go red. See [`assert:`](control-flow.md#assert-self-verification--steps-test).

## Writing pipelines

| Page | What it covers | Length |
|---|---|---|
| [resources.md](resources.md) | Resources and resource types: the built-in `git`, the `check`/`in`/`out` contract, `version:`, `trigger:` | short |
| [expr.md](expr.md) | Expression resource types: `expr:`, the batched `http()`, `env()`/`file()`, when to use it instead of shell | medium |
| [control-flow.md](control-flow.md) | `when:` guards, hooks, `to:`/`verdicts:` routing, `do:`/`in_parallel:`/`race:`/`across:`, `assert:`, approvals | long |
| [agents.md](agents.md) | Agent steps: tools, prompts, verdicts, sub-agents, `fix:`, budgets, failover, CLI agents, ensembles | long |
| [attempts-timeout.md](attempts-timeout.md) | `attempts:` and `timeout:`, and how they interact | short |
| [workspace.md](workspace.md) | `inputs:`/`outputs:`, per-step isolation (always on), read-modify-write artifacts | short |
| [infra.md](infra.md) | Containerized execution (`image:`) and cross-job triggers (`steps watch`) | medium |
| [templating.md](templating.md) | `{{ }}` in resource commands and custom tools, `shellquote`, `((var))`, `load_var:` | short |
| [mcp.md](mcp.md) | MCP servers as tool sources and as resource-type backends | medium |
| [complete.md](complete.md) | A full pipeline putting the pieces together | short |

## Reference

| Page | What it covers |
|---|---|
| [web.md](web.md) | The browser UI: run transcripts, the dependency graph, live runs, triggering |
| [agents-internals.md](agents-internals.md) | How agent steps work underneath: transport, tool-call repair, compaction, caching |
| [conformance.md](conformance.md) | Which Concourse behaviors steps matches, which it doesn't, and which are verified |

## Commands

```
steps run <pipeline>        run one job (--resume <id> continues a failed one,
                            --replay <id> --from <step> re-runs one step of one)
                            --worker <tag>=<url> places tags: steps on a machine
steps watch <pipeline>      poll trigger: true resources, run affected jobs
steps test <pipeline>       run every job and check assert: directives
steps web <pipeline>...     serve the browser UI, polling as it serves
steps validate <pipeline>   check the file, and that this machine can run it
steps plan <pipeline>       show what a run would execute vs skip
steps runs <pipeline>       show what past runs recorded (--cost for spend)
steps preflight <pipeline>  check a job's models and MCP servers are live
steps jobs <pipeline>       list jobs the circuit breaker paused, or resume one
steps approvals <pipeline>  list approval: steps waiting for a decision
steps approve <pipeline> <id>
steps reject <pipeline> <id>
steps mcp tools|login       inspect or authorize mcp_servers: entries
steps docs [page]           read these docs in the terminal
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

`steps preflight` answers the run-time version of the same question: not "is this pipeline runnable" but "is it runnable *right now*". It sends a minimal request to every model the job reaches and starts every MCP server it grants.

**`steps run` does this automatically, before any step executes.** A plan like `plan -> code -> check -> review -> publish` used to discover a dead model half an hour in, with everything before it paid for and thrown away. Now it fails in seconds, saying explicitly that nothing ran:

```
$ steps run pipeline.yml --job self-build
steps: error: job "self-build": preflight failed, no steps were run:
  agent "coder": model "deepseek-v4-flash": no response within 30s
    (other models on this endpoint responded — the model itself looks unavailable, not the endpoint or the key)
```

Tuning, all optional:

```yaml fragment
defaults:
  preflight:
    disabled: false   # true skips it entirely
    timeout: 30s      # per check
    cache: 5m         # a target verified this recently is trusted

agents:
- name: coder
  preflight: false    # opt one agent out — e.g. a local model slow to WAKE
```

The cache is what makes this usable under `steps watch`: without it every poll interval would pay for a probe request against every model. `--no-preflight` skips it for one invocation. **What it does not do:** preflight catches "broken before we start", not "breaks halfway through" — failing over mid-run is [`fallback:`](agents.md#failover-fallback)'s territory.

Add `--syntax-only` to `steps validate` to check the file alone — the right flag for a pre-commit hook or a CI lint that should not need the pipeline's production credentials on hand.

## Editor support

[`steps.schema.json`](../steps.schema.json) is a JSON Schema for the pipeline format. Point your editor at it with a modeline on the first line of a pipeline:

```yaml fragment
# yaml-language-server: $schema=./steps.schema.json
```

That gives completion and inline errors while you type. `steps validate` remains the authority — it checks rules a schema can't express, like whether a `to:` target exists in the same segment — but the schema catches misspelled keys at the keystroke rather than the run.

## Re-running one step: `--replay`

Agent steps are never content-cached, and unskippable propagates forward — so editing the *last* agent step's prompt re-runs every step before it, at full price. That is the single most expensive thing about authoring an agent pipeline.

```bash
steps run pipeline.yml --replay r-8f2a1c --from synthesizer
```

That forks the recorded run and executes from `synthesizer` onward. It does **not** consult the merkle cache: state comes from the source run's workspace (the artifacts earlier steps produced are already on disk), its recorded `run_context`, and its step record.

- **It forks, never mutates.** The source run stays exactly as it was, so the thing you are comparing against still exists — two prompt variants become two runs you can read side by side. `steps runs --cost` prices both.
- **`--from` names a step**, matched against the *current* plan. The pipeline has almost certainly changed since the source run; that is why you are replaying.
- **A source run that never completed an earlier step is refused**, naming it.
- **It needs the source workspace**, so the run being replayed must have been kept (`--keep-workspace`). A reaped tree is a clear error, not a silent full re-run.
