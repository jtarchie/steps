# MCP Servers

MCP (Model Context Protocol) servers are a third kind of external system, alongside LLM providers (`agents:`) and shell-backed resource types (`resource_types:`): a reusable, named connection (`mcp_servers:`) that an agent's `tools:` grant can draw tools from, and/or a resource type's `check`/`in`/`out` can call instead of shelling out.

**Two transports**: Streamable HTTP (`endpoint:`) or a local subprocess over stdio (`command:`). The agent-facing and stdio examples on this page validate but are not executed by the docs suite — they need real servers and credentials. The resource-type examples execute under the suite against an in-process MCP server standing in for the vendor's, so the endpoints they show are illustrative but the check/in/out behavior they demonstrate is tested.

## Declaring a server

```yaml noexec=credentials
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }

- name: linear
  endpoint: https://mcp.linear.app/mcp
  auth: { type: oauth }

agents:
- name: triager
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: github
    tool: search_issues

jobs:
- name: triage
  plan:
  - agent: triager
    messages:
      - "Find open issues labeled 'crash' and summarize them."
```

- `name` is how `agents:` tool grants and `resource_types:` `mcp:` blocks reference this server — declared once, shared by any number of consumers, the same "reusable top-level block" idiom as `agents:`/`resources:`.
- `endpoint` must not embed userinfo (`https://user:token@host/`), since it's folded into cache-hashed content — use `auth.api_key_env` for a credential, never the endpoint itself. Checked at load.
- `auth.type` is `"none"` (default, when `auth:` is omitted entirely), `"bearer"`, or `"oauth"`. `"bearer"` requires `api_key_env` — a static token read from that OS environment variable at run time, exactly like an LLM `agents:` entry's `api_key_env` (the value is never stored in YAML or hashed; only the env var *name* is).
- `api_key_env` (and `oauth`'s `client_secret_env`) may name **any** environment variable of the machine running the pipeline; there is no allowlist. That is consistent with `steps pipeline set` being a remote shell on that machine: whoever can upload a pipeline can already run commands there and read its environment, so restricting these names would add no boundary. Treat upload access to a daemon as access to every secret in its environment.

## Local (stdio) servers

```yaml noexec=stdio-mcp
mcp_servers:
- name: gopls
  command: gopls          # looked up on PATH; argv, never `sh -c`
  args: [mcp]             # optional
  cwd: repo               # optional — see below

agents:
- name: coder
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - read_file
  - mcp: gopls

resources:
- name: repo
  type: git
  source: { uri: https://github.com/jtarchie/ci.git }

jobs:
- name: analyze
  plan:
  - get: repo
  - agent: coder
    inputs: [repo]
    dir: repo
    messages:
      - "Find unused exported functions."
```

A `command:` server is a local subprocess `steps` spawns and speaks newline-delimited JSON to over stdin/stdout, instead of connecting over HTTP.

- **Exactly one of `endpoint:`/`command:`.** Setting both, or neither, is a load error. `args:`/`cwd:` are only valid alongside `command:`.
- **`command:`/`args:` is explicit argv, never a shell.** `command: gopls mcp` is wrong — the whole string is looked up as one executable name; use `args: [mcp]`. There is no globbing, piping, or `&&`.
- **`cwd:` is relative to the agent step's working directory, or absolute.** An **absolute** path is used verbatim. A **relative** path is resolved against the working directory of the agent step whose tools are being built, which is what lets a server be pointed at an input artifact: `cwd: repo` aims a language server at the same materialized tree the agent's own file tools read and edit. **Unset** inherits the directory `steps` was invoked from.

  A relative `cwd:` is rejected at load time for a server backing a **resource type's** `mcp:` config: a `check`/`in`/`out` has no agent step to resolve against. Those need an absolute `cwd:`.
- **Auth is `none` only.** A stdio server has no HTTP request to attach a bearer token to; `auth:` set to anything else alongside `command:` is a load-time error, and `steps mcp login` never applies.
- **Environment is filtered, not inherited.** The subprocess sees only the same host-command allowlist every other host-executed command gets (`PATH`, `HOME`, locale, `TMPDIR`/`USER`/`SHELL`, proxy vars) — **not** the operator's full environment, and specifically not any configured agent's `api_key_env` secret, nor `SSH_AUTH_SOCK`. Steps' build metadata (`STEPS_RUN_ID` …) is not set either: a server has no run and outlives commands. There is no pass-through mechanism for stdio servers, so **a stdio server that needs a credential or any other ambient variable (e.g. `GOFLAGS`, `GOPRIVATE`) is not supported**.
- **Lifecycle**: for an agent tool grant, one subprocess is spawned per agent step per grant and reaped when the step ends. For a resource-type `mcp:` backend, a fresh subprocess is spawned **per** `check`/`in`/`out` call — including every `steps web` poll — fine for a fast-starting binary, a poor fit for one that's slow to start.
- **Diagnostics**: the subprocess's stderr is logged at debug level (`--log-level=debug`), under `mcp.stdio.stderr` with the server's name attached — the only way to see why a stdio server failed to start.

## Taking inventory: `steps mcp list`

What servers does this pipeline configure, who uses each one, and are they working *here*?

```bash
steps mcp list pipeline.yml
```

```
NAME    TRANSPORT  TARGET                              AUTH                USED BY        STATUS
github  http       https://api.githubcopilot.com/mcp/  bearer $GITHUB_PAT  agent triager  ✗ $GITHUB_PAT is not set (auth.api_key_env)
linear  http       https://mcp.linear.app/mcp          oauth               (unused)       ✓ 24 tools
gopls   stdio      gopls mcp (cwd: repo)               none                agent coder    · not probed (cwd: repo resolves per step)
```

- **It connects, by default.** The file says a server exists; only a connection says it works — the binary is on this machine's PATH, the bearer token's env var is set, the oauth token is still good. Each server is probed concurrently, bounded by the same `defaults.preflight.timeout` a run's preflight uses.
- **A server whose credential is already missing is not dialled.** An unset `api_key_env` or a command that is not on `PATH` is knowable without a connection, and a probe that cannot authenticate would report the same problem a second time in whatever words refused it. One problem in two wordings reads as two problems.
- **`--offline` skips every probe**, and still prints `STATUS` — with exactly what is knowable from the file plus this machine: a stdio binary found or not found on `PATH`, a bearer variable set or unset, an oauth server that needs a login. Use it when you want the inventory instantly, or on a machine that holds none of the credentials.
- **A relative `cwd:` is listed but not probed** (`·`). It resolves against the agent step's own working directory, which only exists while a run is happening — spawning it from your shell's directory instead would report a working server as broken.
- **`USED BY` is the consumers**: the agents whose `tools:` grant the server, the `fix:` blocks whose own `tools:` grant it, and the resource types whose `mcp:` backend calls it. `(unused)` means nothing in this pipeline can reach it.
- **`AUTH` names the env var, never the value** — `bearer $GITHUB_PAT` is the variable to go check.
- **It reports; it does not gate.** A server that doesn't answer is a row with an ✗ and a zero exit status. `steps validate --live` is what fails on one, and plain `steps validate` is what checks the file.

## Discovering a server's tools: `steps mcp tools`

Before writing a tool reference or a resource type's `mcp:` block, find out what a server actually exposes — its tool names and argument schemas are not something to guess:

```bash
steps mcp tools pipeline.yml github
```

This connects (per the server's configured auth) and prints each tool's name, description, and argument schema. It works for any auth type — stdio included — and doubles as a connectivity/auth smoke test.

## Granting MCP tools to an agent

A `tools:` entry can reference an MCP server in three progressively broader forms. Each grant opens one connection to the server for the step, shared by every tool it selects:

```yaml noexec=credentials
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }

agents:
- name: triager
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: github
    tool: search_issues            # one tool — may also set description/required/max_calls
    max_calls: 5
  - mcp: github
    tools: [get_issue, list_pulls] # a named subset — each keeps its server-advertised description

jobs:
- name: triage
  plan:
  - agent: triager
    messages:
      - "Triage today's crash reports."
```

- **Single tool** (`tool:`): the only form that may also set `description:` (overriding the server's own), `required:`, `max_calls:`, or `args:` (see [Pinning arguments](#pinning-arguments-args) below) — the same semantics as a custom tool (see [agents.md](agents.md)). Its model-facing function name is `<server>__<tool>` (double underscore — a dot is rejected by OpenAI's function-name charset).
- **Named subset** (`tools:`): a list of tool names, each exposed under its own `<server>__<tool>` name. `description:`/`required:`/`max_calls:`/`args:` are load-time errors here — they're single-tool concepts.
- **Bare form** (`- mcp: github`, neither set): every tool the server currently exposes.
- **Grant, not inline**: like a sub-agent tool, an MCP grant must live on the `agents:` entry (or a `fix:`'s own `tools:` override) and be selected by bare name (`tools: [github]`) on a step — a step cannot introduce `{mcp: ..., tool: ...}` inline.
- **Cache hashing**: a server's endpoint/auth type/`api_key_env` name (never a value) — or, for a stdio server, its `command`/`args`/`cwd` — and the granted tool name(s) fold into the step's hash. The bare "all tools" form hashes as a static marker, since plan-time hashing has no live connection to enumerate the server's tools; use the explicit `tools: [...]` form if you want the server's tool list changing to bust the cache.
- **Tool results**: translated to the same map shape every other tool returns — a transport failure or a result with `isError: true` becomes `{"error": ...}`; a successful call becomes `{"structured_content": ..., "content": ...}`.

### Pinning arguments: `args:`

Which tools exist is one lever; which project, database or repo they reach is the other. Written into a prompt ("project 307, never another"), that scope is a request the model is free to ignore. `args:` on a single-tool grant makes it a boundary:

```yaml test=mcp-pinned-args mcp=mcp-pinned-args
mcp_servers:
- name: honeybadger
  endpoint: https://mcp.honeybadger.io/mcp
  auth: { type: oauth }

agents:
- name: oncall
  source: { model: openrouter/qwen/qwen3.7-flash }
  tools:
  - mcp: honeybadger
    tool: list_faults
    args:
      project_id: 307        # removed from the schema the model sees, and
                             # sent as 307 whatever the model asks for

jobs:
- name: triage
  plan:
  - agent: oncall
    messages:
      - "What is failing right now?"
    assert:
      tool_calls:
      - name: honeybadger__list_faults
        args: { q: timeout }  # only model-authored args are assertable —
                              # naming the pinned project_id is a load error
  assert:
    execution: [oncall]
    outcome: succeeded
```

- **The model can neither see nor set a pinned key.** It is removed from the tool's advertised `properties` and `required` before the schema reaches the model, and merged **over** whatever the model sends before the call goes over the wire. A model that sends one anyway — or a case variant of one, like `Project_ID`, which a Go server's case-insensitive JSON decoding could bind instead — has it overwritten or dropped, and the run's transcript gets a warning naming the key (never the value it tried). A case variant the tool really does declare under that exact spelling is left alone.
- **A pin must bind, or the step is refused.** Each key must be a top-level property the tool's input schema declares; a pin the server does not declare, or on a schema with no `properties` at all, fails with the list of what it does declare. `$ref`/`allOf` schemas and nested paths are not resolved, so a pin into one is refused rather than guessed at. Preflight checks this before any job starts (and `steps validate --live` reports it); every step checks it again when it connects, so a server that changed its schema since is caught too. Plain `steps validate` has no connection and checks only the shape.
- **Values are sent as the declared type.** `integer`, `number` and `boolean` properties get the pinned string parsed (`307` arrives as the number 307), including through a nullable `type: [integer, "null"]` or a single non-null `anyOf`/`oneOf` branch; an `object`, `array` or `null` property cannot be pinned from a string and is refused; anything else is sent as the string. A value that does not parse is refused the same way an undeclared key is. Quoting in YAML is optional.
- **Single-tool form only**, like `description:`/`required:`/`max_calls:` — a pin is per-tool by nature.
- **A pin binds the grant it is written on, and nothing else.** Another grant of the same server in the same `tools:` list that also offers the tool — the bare form, a `tools:` subset naming it, the same `tool:` again — would hand the model the same function unpinned, so it is a load error. A sub-agent's or a `fix:`'s own grant of the same server is a separate capability you chose; pin it there too if it needs the same scope.
- **Aliases are invisible.** A server that accepts the same parameter under two names (`project_id` and `projectId`) will take the unpinned one; pin or withhold both.
- **Not for secrets.** Pinned values fold into the step's hash and land in the state database, exactly as a custom tool's `args:` do, and a changed pin re-runs the step. Credentials belong in the server's `auth:`.

## Sharing tools with other agents: `mcp_exports:`

A daemon can serve some of a pipeline's tools to agents somewhere else — a teammate's Claude or Cursor — as an MCP server of its own. The calls go out with the pipeline's credentials, which the caller never sees, and every one is recorded under the name of the person who connected.

```yaml noexec=credentials
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }

agents:
- name: oncall
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: github
    tool: search_issues
    args: { owner: acme, repo: app }

mcp_exports:
- name: support
  access: '"acme/support" in Groups'
  rate: 60/m
  image: alpine:3                       # where the commands below run, one fresh container each
  user: nobody
  network: none
  privileged: false
  container_limits: { cpu: 512, memory: 268435456 }
  tools:
  - mcp: github
    tool: search_issues
    description: Search issues in the app repository
    args: { owner: acme, repo: app }   # hidden from the caller, sent whatever it asks
    timeout: 20s
    params:                            # what the caller may send, shown to it and enforced
      query: { max_length: 200, pattern: '^[^;]*$' }
      per_page: { max: 50, default: 20 }
  - mcp: github
    tools: [get_issue]
  - agent: oncall                       # the caller gets an answer, not the tools
    description: Ask what is failing right now
  - name: lookup_owner                  # a command of the pipeline's own
    description: Who owns this service?
    run: grep -i {{ .args.service | shellquote }} OWNERS
    params:
      service: { max_length: 40, pattern: '^[a-z0-9-]+$' }

jobs:
- name: build
  plan:
  - task: say
    run: echo hi
```

- **`access:` is required**, a [role rule](authentication.md#who-may-do-what-roles) about the person connecting. Separate levels of access are separate exports (`support`, `support-write`), so who may do what stays in the file, where it is reviewed.
- **MCP grants, sub-agents and custom `run:` tools.** A built-in has no workspace outside a step, so it is a load error. `max_calls:` and `required:` are about a step's conversation, which an export does not have; `rate:` bounds calls per person instead. `max_output_bytes:` works, and an over-long result is cut at the limit with a note — there is no file for a caller to read the rest from.
- **Narrowing works as it does for an agent**: `args:` pins (single-tool form), `description:`, `timeout:`.
- **`params:` limits what a caller may send**, per argument: `max_length`, `min_length`, `pattern`, `enum`, `min`, `max`, `default`. They are written into the tool's input schema, so Claude or Cursor reads "at most 200 characters" and corrects itself before calling, and the daemon enforces the same limits whether it did or not: a call that breaks one is refused with a tool error naming the argument (never its value) before anything is sent upstream. `default` fills an argument the caller left out. The same `params:` works on an agent's own grant of the tool — see [agents.md](agents.md#params--limits-on-what-an-argument-may-be). Whether a parameter is a number comes from the server's own schema, so `min`/`max` on a string parameter is refused when the export is first built.
- **A custom command** is written as it is for an agent (`name`, `description`, `run`, `args:` pins, `timeout:`), and the caller only supplies values. Every value must go through `shellquote` — `{{ .args.note | shellquote }}` — so a caller sending `$(id)` gets that text, not a command. It must be a bare word the shell reads once: `echo "{{ .args.note | shellquote }}"` is refused, because inside quotes of your own the quoting no longer holds, and so is any command that uses a heredoc (`<<`), backticks, `$'…'`, `eval` or a nested `sh -c`, where the shell expands the value a second time. A value that is not quoted, or sits inside quotes, is refused when the pipeline is set or validated, and again when the export is built. Each call gets a fresh working directory, removed when it ends, so two callers never see each other's files. The command sees the daemon's baseline environment and the variables it names in its own `env:` (below), never the pipeline's `env:` or another tool's.
- **`image:` runs those commands in a container** — a fresh one per command, over the call's directory — with the same `user:`, `network:`, `privileged:` and `container_limits:` a task takes (see [infra.md](infra.md)), which need `image:` as they do there. Without it they run on the daemon's machine as whoever started it, so name an image for anything you would not run yourself. Those settings are refused on an export that has no custom command to apply them to.
- **A sub-agent export** runs that agent's own conversation for each call, with your model and your tools, and returns its answer — the narrowest thing to share.
- Never hashed: an export changes nothing a build does.

### A command's credentials: `env:`

A custom command that has to authenticate to something names the variables it needs:

```yaml noexec=credentials
mcp_exports:
- name: deploys
  access: '"acme/eng" in Groups'
  tools:
  - name: deploy_status
    description: What is deployed for this service?
    env: [DEPLOY_API_TOKEN]
    run: 'curl -fsS -H "Authorization: Bearer $DEPLOY_API_TOKEN" https://deploy.example.com/status/{{ .args.service | shellquote }}'
    params:
      service: { max_length: 40, pattern: '^[a-z0-9-]+$' }
  - name: services
    description: Which services are there?
    run: cat SERVICES

jobs:
- name: build
  plan:
  - task: say
    run: echo hi
```

- **Names only**, read from the daemon's environment at each call, as every `env:` is. Each tool lists its own: `services` above never holds the token.
- **Their values are masked** in what the caller gets back — stdout, stderr, an error, a rule's reason — replaced by `***` wherever the exact text appears, so a `curl -v` or an error echoing a header returns the mask. Only the exact text: a value the command transformed (base64 in a Basic auth header, URL-encoding, a value cut in half by `max_output_bytes:`) is not recognized, so a command should still not print what it was given.
- **With `image:`**, the variables are passed into the container as a task's are.
- **Refused** on an MCP grant or a sub-agent, which authenticate through their own `mcp_servers:` or agent entry, and on any tool an agent grants or a step selects, whose command sees the agent's own `env:`.

### Rules: checks on each call

`params:` limits what one argument may be; a **rule** decides about the whole call — who is asking, what they are asking for, what came back. A rule is named once at the top of the pipeline and attached by name to an export (it checks every tool in it), to one tool in it, or to a tool an [agent is granted](agents.md#rules--checks-on-each-call).

```yaml noexec=credentials
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }

agents:
- name: triage-judge
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  system: You decide whether a call may go ahead. Reply allow or deny, then one short reason.

rules:
- name: acme-only
  when: args.owner in who.Organizations
- name: no-secrets
  accept: not (result.text matches "(?i)private key|api[_-]?key")
- name: sanity
  judged_by: triage-judge

mcp_exports:
- name: support
  access: '"acme/support" in Groups'
  rules: [no-secrets]             # every tool in the export
  tools:
  - mcp: github
    tool: search_issues
    args: { owner: acme }
    params:
      query: { max_length: 200, pattern: '^[^;]*$' }
      limit: { max: 50, default: 20 }
    rules: [acme-only, sanity]    # this tool, after the export's

jobs:
- name: build
  plan:
  - task: say
    run: echo hi
```

- **`when:`** runs before the call and **`accept:`** after it, each an [expression](expr.md) that must come out `true`. **`judged_by:`** asks one of the pipeline's agents to answer `allow` or `deny`. The order is: the export's rules, then the tool's; within them every `when:`, then every judge, then the call, then every `accept:`. The cheapest check goes first, so an agent is paid for only by a call nothing cheaper refused.
- **What an expression sees**: `tool`, `args` (as the tool will receive them — pins and `params:` defaults applied, so `args.owner` above is `acme` whatever the caller sent), and `who`: the person calling, with `Provider`, `UserID`, `ID`, `Email`, `Organizations` and `Groups`, the same things the export's `access:` rule reads. `accept:` also sees `result` — `result.text` is exactly what the caller would receive, `result.content` the same as an object, `result.error` the call's own error. Expressions are pure: no `http()`, `env()` or `file()`. They are compiled when the pipeline is validated or set, so one that cannot compile is refused there.
- **A refusal tells the caller why.** `fail("why")` gives the reason itself; a plain `false` says "refused by rule <name>"; a judge's reason is passed on. A rule that errors, or a judge that fails, times out or answers neither word, refuses — nothing is let through on a rule's say-so that it did not give. The caller sees a tool error and the upstream is never reached by a `when:` or judge refusal; an `accept:` refusal withholds the result, but the call has already run.
- **Refusals are recorded under the rule's name**: the activity log shows `mcp refused` with the rule, never what was asked, and the export's page counts the day's refusals apart from the calls that ran.
- **A judge** is a hosted agent of the pipeline, shown the tool, who is asking and the arguments (framed as data, not instructions), and bounded to two minutes. Its tokens are the pipeline's, so give it a small model and a short `max_turns`. It is not itself judged, and cannot reach itself: a judge whose own tools carry a judged rule, or that can reach itself through the agents it grants or the judges of its tools' rules, is a load error.

### Where an export is seen

A pipeline's **mcp** tab lists its exports under its servers — what it shares beside what it connects to — with how many tools each offers, how many people and agents are connected, and how many calls came in today. Each export has a page of its own: its address with a copy button, its access rule as written, step-by-step setup for each app that can connect — Claude Code, Cursor, VS Code, Codex, Gemini CLI, Windsurf, Zed, Claude and ChatGPT, with a one-click install where the app has one — every tool with its fixed arguments and time limit, and its most recent calls. Nothing on it is editable: the pipeline file is where an export changes.

### Connecting an agent

An export's address is `<external-url>/mcp/x/<pipeline>/<export>`. Give it to an agent that speaks MCP over HTTP with OAuth — Claude, Claude Code, Cursor — and it does the rest: it finds the daemon's sign-in through the address's [protected-resource metadata](https://www.rfc-editor.org/rfc/rfc9728), registers itself, and sends you to the daemon to sign in and agree.

**Claude and ChatGPT connect from their vendors' servers, not your machine**, so the address must be reachable from the internet — and, for ChatGPT, `https`. On a daemon whose `--external-url` is this machine or a private network address, the export's page shows their tabs disabled and says why; Claude Code, Cursor and the other desktop tools only need the address to answer from where you sit.

The daemon asks every time, on a page that says plainly what is being agreed to:

- **the client's name, marked unverified** — it is whatever the agent called itself when it registered, and anybody can register;
- **where your browser will be sent** with the result, the most prominent line on the page;
- **which export, and every tool in it**, with the arguments fixed for you;
- that calls are made with the pipeline's credentials and recorded under your name.

**Deny** has the focus. Nothing is issued until you press **Allow**: a site that registered itself cannot collect a token by sending your signed-in browser to the daemon, because all that browser gets is this page.

- You must be allowed the export: its `access:` rule, the pipeline's own [`access:`](authentication.md#one-pipelines-own-rules-access), and a role on the daemon at all. Anybody else is told so, with nothing to press.
- **PKCE (S256) is required**, and the agent must name the export it wants (the `resource` parameter); the daemon refuses anything else back to the agent.
- **Tokens are short and checked again.** An access token lasts an hour. Refreshing it asks again whether you may use the export — leave the team that let you in and your agent loses it at its next refresh — and hands out a new refresh token, ending the old one. Tokens are kept on the daemon only as digests.
- **Registration is open and bounded**: an agent may register return addresses that are `https`, `http` to the same machine, or an app's own scheme, never one that runs something in the browser; the daemon keeps at most 1,000 registered agents.
- Agreeing is recorded in the [activity log](web.md#activity) as `mcp authorize`.

### What a connected agent can do

- **See and call the export's tools, and nothing else**, under the names an agent's grant would use (`github__search_issues`). A pinned argument is not in the schema it sees, and is sent as written whatever it asks for.
- **Every call is recorded** in the activity log as `mcp call`, under the name of the person who agreed: which tool, the *names* of the arguments it sent, and whether it worked. Never the values — a caller can paste anything into a query.
- **Access is checked on every request**, not only when a token is refreshed: an export a later `steps pipeline set` narrows, or a team the person left, stops their agent at its next call.
- **`rate:`** counts each person's calls to the export; a call past it comes back to the agent as the tool's error, which it can read and wait on.
- A token is for one export: it opens nothing at another, even in the same pipeline.
- Exports are served only by a daemon people sign in to, and not by a `--read-only` one, since calling a tool is acting.

### Disconnecting an agent

**connections**, in the menu under your name, lists every agent you have let connect: the name it gave itself (unverified), where it sends people back, which export, when you agreed, when it last called and how many times. **Revoke** asks once more, then ends every token the agent holds for that export — its next call fails, and it cannot refresh its way back. An admin can switch the page to **everyone** and disconnect anybody's agent. Both are recorded in the activity log as `mcp disconnect`.

## Backing a resource type with MCP

```yaml mcp=mcp-linear-issues
mcp_servers:
- name: linear
  endpoint: https://mcp.linear.app/mcp
  auth: { type: oauth }

resource_types:
- name: linear-issues
  config:
    mcp:
      server: linear
      check:
        tool: list_issues    # called with the source below as its arguments;
                             # must return an oldest-first JSON array of
                             # version objects
      out:
        tool: create_issue   # optional — enables `put`; called with the put's params:

resources:
- name: eng-bugs
  type: linear-issues
  source:                    # IS list_issues' argument object (see args: below)
    team: ENG
    label: bug

jobs:
- name: react
  plan:
  - get: eng-bugs
    trigger: true
  - task: record
    inputs: [eng-bugs]
    run: cat eng-bugs/version.json
    assert:
      stdout: ENG-204        # the suite's server reports ENG-101 then ENG-204,
                             # oldest first — so the get took the LATEST
  assert:
    execution: [eng-bugs, record]
    outcome: succeeded
```

- **Every stage is optional, but a block must do something.** A type declaring none of `check:`/`in:`/`out:` is a load error. A `get` against a type with no `check:` is one too, and so is a `put` against a type with no `out:` — which is what makes a **publish-only type** (all `out:`, no `check:`) a first-class shape: the half of a workflow that posts a reply has no versions to discover, and naming a check tool nothing ever calls would be a ritual. `mcp:` is mutually exclusive with the shell `check:`/`in:`/`out:` strings — a resource type sets one style or the other, which is how a pipeline **detects over HTTP and acts over MCP**: two resource types, one shell-backed and one MCP-backed.
- **The arguments are the remote tool's own schema.** A tool publishes the parameters it takes and rejects a call that omits a required one, so steps sends exactly what you name and wraps it in nothing: `check` and `in` send the resource's `source:`, `out` sends the put's `params:`. Run `steps mcp tools <pipeline> <server>` to see what a tool actually requires before writing either.
- **`check`**'s result must be an oldest-first JSON array of version objects, accepted either as structured content or a single text block containing that array — as close a mirror of the shell path's "stdout is a JSON array" convention as an RPC result allows.
- **`in`, when omitted** (the common case — detecting new issues, not fetching their content): `get` just writes the selected version object to `<resource>/version.json`, no MCP call. **When set**, its result is materialized into the get's directory: structured content as `result.json`, each content block as `content-N.<ext>`. `version.json` is always written either way.
- **`out`, when set**, is what a `put` calls; `params:` on the step carries the payload. A `put` targeting an MCP-backed type with no `out:` is a load-time error naming the missing tool.

### Naming the arguments: `args:`

`source:` and `params:` only work as arguments when their keys already match the tool's parameters. When they don't — or when the value the tool needs lives on the **version** a check produced, which is the usual shape for `in:` — name the mapping instead. Every string in it is a template over exactly what that stage has, the same as a shell `check`/`in`/`out` command: `check` renders against `{source, version}`, `in` against `{source, version, params}`, `out` against `{source, params}`.

`check`'s `version` is the [check cursor](resources.md#the-check-cursor) — the version the last successful check reported — and `args:` is the only way to reach it. A check with no `args:` still sends the `source:` alone: that payload is the tool's own published schema, and no third-party server declares a parameter steps invented.

```yaml mcp=mcp-slack-thread
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth }

resource_types:
- name: slack-thread
  config:
    mcp:
      server: slack
      check:
        tool: slack_search_public_and_private   # source: is already {query: ...}
      in:
        tool: slack_read_thread                 # needs the version's fields
        args:
          channel_id: "{{ .version.channel }}"
          message_ts: "{{ .version.ts }}"
      out:
        tool: slack_send_message
        args:
          channel_id: "{{ .params.channel }}"
          thread_ts: "{{ .params.thread_ts }}"
          message: "{{ .params.text }}"         # the tool calls it `message`

resources:
- name: mentions
  type: slack-thread
  source:
    query: "to:me is:thread"

jobs:
- name: answer
  plan:
  - get: mentions
    trigger: true
  - task: read
    inputs: [mentions]
    run: cat mentions/content-0.txt   # slack_read_thread's text result
    assert:
      stdout: C0123456789 at 1717171717.000100   # the version's own fields,
                                                 # delivered through in.args:
  - put: mentions                     # answer in the thread the check found
    params:
      channel: C0123456789
      thread_ts: "1717171717.000100"
      text: on it
  assert:
    execution: [mentions, read, mentions]
    outcome: succeeded
```

- **`args:` replaces the default payload entirely** — it is the argument object, not an addition to it.
- **Non-string values pass through untouched**, so `limit: 20` reaches the tool as the number 20. Templates nest through mappings and lists.
- **A template naming a field that isn't there fails the step**, exactly as a shell command's template does — a typo'd `{{ .version.chanel }}` is not quietly nothing. In `check.args:` that bites on the very first poll, where the cursor is an empty map: write `{{ index .version "ts" | default "0" }}`, never a bare `{{ .version.ts }}`.
- **A templated value is always a string.** `limit: 20` stays the number 20, but `limit: "{{ .source.limit }}"` sends `"20"`. Numbers lifted out of a version keep their exact digits — an issue id of `123456789` renders as `123456789`, not in exponent form.
- **`{file: ...}` markers in a `put`'s `params:` are resolved first**, so `{{ .params.text }}` renders the file's contents.
- **A template renders a string, so no `{{ .source }}` emits a whole object.** A tool that wants a nested object gets it enumerated: `args: { filter: { team: "{{ .source.team }}" } }`.
- **`args:` is part of the get's or put's hash**, so changing a mapping re-runs it rather than reusing what the old one fetched.

### When a tool returns prose: detect over HTTP, act over MCP

A `check` needs a machine-readable list, and not every MCP tool has one to give. Many vendor servers answer with Markdown written for a model to read — Slack's search returns `{"results": "# Search Results for: …", …}`, with no `structuredContent` and no published `outputSchema`. There is no list in there to select, at any nesting depth, so no mapping can make that tool a `check`. (This is not a niche accident: until [SEP-2106](https://modelcontextprotocol.io/specification/) `structuredContent` was required to be a JSON *object*, so an array-returning tool was not even legal.)

The split that does work: let the vendor's HTTP API do the **detecting**, where the response is JSON with stable ids, and let MCP do the **acting**, where prose in the response costs nothing. Two resource types, one of each style:

```yaml noexec=credentials
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth }

resource_types:
- name: slack-mentions          # DETECT: real JSON, stable {channel, ts}
  env: [SLACK_BOT_TOKEN]
  config:
    check: |
      channel={{ .source.channel | shellquote }}
      curl -sS -H "Authorization: Bearer $SLACK_BOT_TOKEN" \
        "https://slack.com/api/conversations.history?channel=$channel&limit=20" |
      jq -c --arg ch "$channel" '[.messages[] | {channel: $ch, ts: .ts}] | reverse'

- name: slack-reply             # ACT: publish-only, no check: to write
  config:
    mcp:
      server: slack
      out:
        tool: slack_send_message
        args:
          channel_id: "{{ .params.channel }}"
          message: "{{ .params.text }}"

resources:
- name: mentions
  type: slack-mentions
  source: { channel: C0123456789 }
- name: reply
  type: slack-reply
  source: {}

jobs:
- name: answer
  plan:
  - get: mentions
    trigger: true
  - task: compose
    inputs: [mentions]
    outputs: [msg]
    run: echo "on it" > msg/body
  - put: reply
    inputs: [msg]
    params:
      channel: C0123456789
      text: { file: msg/body }
```

The rule of thumb: **a tool whose output a model was meant to read is an agent's tool, not a resource's.** Grant it to an `agent:` step, where prose is exactly right, and keep `check:` on something that returns ids.

### Preflight checks this before anything runs

Both ways an MCP call is wrong — a tool the server doesn't expose, and required arguments the call will never send — are answerable from the server's published tool list, without calling anything. So they are: `steps run` checks the resources its job touches before the first step, `steps validate --live` asks the same question on demand, and **`steps web` checks every `trigger:` resource of a pipeline before polling it, and leaves the pipeline unpolled if one can't work** — logged as `trigger.unpollable`, until a `steps pipeline set` changes the configuration. That last one is the point: a poll loop's reaction to a permanent misconfiguration is to log it and try again on the next interval, forever, with nothing enqueued and nothing red. A problem that waiting can fix — a server that did not answer, a token a refresh would renew — is logged once as a warning and left to the poll loop.

```
trigger preflight failed, nothing was polled:
  resource "mentions": check tool "slack_search_public_and_private" requires [query], which this call does not send (it sends: [to])
    (the resource's source: IS the argument object when mcp.check.args: is unset — name it there, or map it in mcp.check.args:)
```

`--no-preflight` skips it, as it does for models.

A shallower check runs earlier still, needs no network at all, and cannot be skipped: an `mcp_servers:` entry whose stdio `command:` is not on this machine's `PATH`, or whose `auth.api_key_env` names a variable that is unset or empty, is a **preflight problem** — the same treatment an agent's `api_key_env` gets. `steps validate` reports it, and `steps pipeline set` REFUSES a pipeline that has one, so a daemon never serves a pipeline whose tooling is already known to be broken. An `auth: {type: oauth}` server is deliberately not checked this way: its credential is a saved login, and refusing the set would leave nothing to log in against.


### Sending a file's contents: `{file: ...}` in `params:`

A shell `out:` runs with the put's read view as its working directory and reads what it needs. An MCP `out:` is a tool call with no working directory, so a value a *previous step wrote* needs a way in — otherwise the payload could only ever be text the pipeline author typed, which rules out publishing anything an agent produced.

A `params:` mapping whose **only** key is `file` is replaced by that file's contents:

```yaml mcp=mcp-file-params
mcp_servers:
- name: linear
  endpoint: https://mcp.linear.app/mcp
  auth: { type: oauth }

resource_types:
- name: linear-issues
  config:
    mcp:
      server: linear
      check: { tool: list_issues }
      out: { tool: create_issue }

resources:
- name: eng-bugs
  type: linear-issues
  source: { team: ENG }

jobs:
- name: file-a-bug
  plan:
  - task: investigate
    outputs: [report]
    run: echo 'the retry loop never backs off' > report/body.md
    assert:
      files: [report/body.md]
  - put: eng-bugs
    inputs: [report]
    params:
      title: Retry loop spins
      description: { file: report/body.md }   # <- contents, not the literal map
  assert:
    execution: [investigate, eng-bugs]
    outcome: succeeded
```

- **The path is relative to the put's read view**, so its first component names an artifact in the step's `inputs:` — the same path shape as `across:`'s `from_file:`. Absolute paths, `../` escapes, and a bare filename naming no artifact are all errors, as is a file that isn't there (which says so, and asks whether its artifact is in `inputs:`).
- **Exactly one key, and a string value.** `{file: report.pdf, title: "Q3"}` is a real object a tool might genuinely define, so it passes through untouched. Only the single-key form is a marker — that strictness is what makes it safe to spell this inside free-form `params:` rather than beside it.
- **It nests.** Markers are resolved at any depth, through mappings and lists alike, so a tool taking structured blocks can have one filled from a file.
- **Contents are trimmed**, exactly as [`load_var:`](templating.md) trims. The usual way to produce one of these files is a redirect (`jq -r .id > meta/id`), whose trailing newline belongs to the writing rather than the value — untrimmed, the first id or timestamp you hand to an API is rejected.
- **Shell `out:` is unaffected** — it already reads the workspace directly, and reinterpreting a `{file: ...}` param there would change what existing pipelines send.
- **Detect vs. respond**: a resource-type `check` feeding `trigger:` is the natural fit for *detecting* something (new issues, a new PR). An open-ended *response* — read context, decide, comment — is usually better as an agent step granted the same server's tools, where the model drives; a deterministic `out:` put (exact fields, no judgment) is the resource-type path.

## Authorizing an oauth server: `steps mcp login`

A bearer-configured server needs no login step — it reads its token from the environment at run time. An oauth-configured server needs a one-time interactive authorization:

```bash
steps mcp login linear -c pipeline.yml                                  # authorize THIS machine
steps mcp login linear -p app --target https://ops:PASSWORD@steps.example.com   # authorize a daemon
```

Either one runs the OAuth 2.1 authorization-code + PKCE flow: discovers the server's metadata, dynamically registers a client, opens your browser, and prints where the token was saved. Which machine ends up holding the token is the whole difference, and it is chosen by which pipeline you name — a file here (`-c`), or one a daemon serves (`-p`). See [Authorizing a daemon](#authorizing-a-daemon) for the second.

- **The authorization URL is always printed**, whether or not the browser opened:

  ```
  Authorize in your browser:

    https://auth.example.com/authorize?client_id=…&code_challenge=…
  ```

  A browser that opens the wrong profile, opens nothing visible, or isn't there at all (SSH, a container) is indistinguishable from success on this side — the flow would just sit and wait with nothing on screen. Copy the URL into any browser that can reach the loopback address printed in `redirect_uri` and the login completes normally.
- **A slightly malformed `WWW-Authenticate` challenge is tolerated.** Some servers separate the challenge's parameters with spaces where the HTTP spec wants commas (Metabase's MCP endpoint does). The header still says exactly one thing, so steps normalizes it and continues rather than failing the login over punctuation.
- **An unadvertised `iss` is tolerated, a wrong one is not.** RFC 9207 has the authorization server return its own identifier on the redirect, and servers are supposed to declare that in their metadata. Some return it without declaring it (Metabase again). steps checks the value against the issuer it discovered: matching is fine and the login proceeds; an `iss` naming a *different* issuer is the mix-up attack the parameter exists to catch, and fails the login by name.
- **Sealed into the state database, under a key you hold**: the token lands in the same database the pipeline's runs are recorded in — the per-file `.steps/<file>.db` locally, the daemon's `--db` on a daemon — encrypted with AES-256-GCM under `STEPS_ENCRYPTION_KEY`. A copy of the database (a backup, a Postgres dump, a laptop's `.steps/`) is a copy of ciphertext.

  ```bash
  export STEPS_ENCRYPTION_KEY="$(openssl rand -base64 32)"   # keep it somewhere that is not the database
  ```

  - **No key, no login.** steps refuses to keep a token it cannot encrypt rather than keep it in the clear: `steps mcp login` (either form) and the web UI's Connect stop before any browser opens and name the variable. A malformed key fails the command at once rather than at the first login.
  - **The key is not in the database and never travels.** It is read from the environment of the process that spends the token — your shell for `-c`, the daemon's for `-p`. Lose it and every saved login reads as unreadable until you log in again under a new one.
  - **Rotating the key.** Set the new key as `STEPS_ENCRYPTION_KEY` and the old one as `STEPS_OLD_ENCRYPTION_KEY`. Each login opened under the old key is re-sealed under the new one the next time it is read; once every server has been used, drop the old variable.
  - **A sealed login opens only in its own pipeline.** It is bound to the pipeline's identity in the database, so a row copied onto another pipeline does not decrypt, a rename keeps it, and a pipeline set under a destroyed one's name starts with none.
- **Per pipeline**: two pipelines declaring `github` log in separately, so each can act as a different account (a bot for one, a person for the other), and a login for one never changes what the other does.
  - **Which pipeline name.** On a daemon, the name it serves the pipeline under (`-p`). Locally, the name the command's own state uses: the file's base name, or `--name <name>=<file>`. `steps mcp login -c`, `mcp list`, `mcp tools` and `validate --live` all take `--name` and `--db`, and `mcp login` prints where it saved.
  - **Two repos, one file name.** Two checkouts that each have `ci/pipeline.yml` keep separate `.steps/` directories, and so separate logins. Two pipelines pointed at one `--db` under one name share one.
  - **A rename keeps the logins; a destroy removes them**, with the rest of the pipeline's history. A token refreshed by a connection still open when its pipeline was destroyed is dropped with a warning: it never brings the pipeline back, and never lands on a pipeline set under the same name since.
  - **Logins saved by an older steps** (`${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<pipeline>/<server>.json`, in the clear) are moved into the state database, sealed, the first time anything reads them, and the file is deleted. With no key set the file is left where it is and the read fails naming the variable. A rename onto a name that still has such a directory is refused, and a destroy removes it.
- **Silent refresh, no re-prompting**: `steps run`/`steps web` never run an interactive flow — they load the persisted token and refresh it silently. If it can't be refreshed, the error names the `steps mcp login` command to run, in both its forms.
- **The refresh grant is registered, not assumed.** Dynamic registration declares `grant_types: [authorization_code, refresh_token]`, because RFC 7591 defaults an omitted `grant_types` to `authorization_code` *alone* — and a conforming server then issues an access token with no refresh token beside it, exactly as asked. That looks like a successful login and dies at the first expiry, unattended.
- **A login that can't be renewed is a failed login.** If the authorization server returns no refresh token for a token that *does* expire, `steps mcp login` saves it (it works right now) and exits non-zero saying when it stops working:

  ```
  steps: error: mcp login: mcp server "metabase": authorized, and the token was saved —
    but the authorization server issued no refresh token, so this credential stops working
    at 2026-08-13T22:44:05-06:00 and `steps run`/`steps web` will fail from then on with
    no way to renew it.
  ```

  A token with no expiry at all is fine and reports nothing: there is nothing to renew.
- **`auth.scopes:` is what gets requested.** Left unset, steps asks for every scope the server's protected-resource metadata advertises, which is the right default for a server the pipeline has no opinion about. Setting it narrows the authorization request to exactly those scopes — worth doing against a server whose scope list includes writes you never intend to make.
- **Trust boundary**: nothing token-shaped is ever cache-hashed, and nothing token-shaped reaches the state database unencrypted. What a database thief gets is ciphertext; what somebody with the running process gets is everything, as it always was — the key is in its environment and the token in its memory.

### Authorizing a daemon

A token belongs on the machine that spends it, and for a pipeline set into `steps web` that is the daemon — often a machine with no browser, which therefore cannot be the loopback address a redirect comes back to. So the daemon runs the flow, and your terminal plays only the browser's half:

```bash
steps mcp login linear -p app --target https://ops:PASSWORD@steps.example.com
```

1. The daemon resolves `linear` from the configuration it is **serving** for `app` — not from a file on your disk — so the token is bound to the endpoint that pipeline will actually dial.
2. It discovers, registers a client whose redirect URI is `<target>/mcp/callback`, and hands the authorization URL back; the CLI prints it and opens your browser.
3. The authorization server redirects the browser to the daemon, which exchanges the code and seals the token **into its own state database**. The PKCE verifier and the token never leave it.
4. The CLI reports the outcome in the flow's own words — including "authorized, but this token cannot be renewed" — and the daemon's next poll or run uses the token. Nothing restarts.

- **The redirect URI is the address you reached the daemon on**, with the credentials taken off first: it is sent to the authorization server and kept there, so a password on it would have been handed to a third party. It is also a *stable* URI, which the loopback flow's ephemeral port never is — a server demanding exact pre-registered redirect URIs gets `https://steps.example.com/mcp/callback`, registered once.
- **`GET /mcp/callback` asks for no password**, one of the few routes (with a [webhook delivery](authentication.md#what-it-covers)) that does not. It cannot: the request is a bare navigation a third party caused. It is authenticated by its `state` — a single-use nonce minted for that one login and seen only by whoever was handed its authorization URL, which takes the role that may start a login to get: a viewer sees that a login is in progress, never its URL. A state nobody is waiting on is a `404`.
- **Starting a login and reading its URL do need credentials**, like everything else under `/api`.
- **One login per pipeline and server at a time.** A saved login is keyed by both, so a second login for the same pair replaces the first rather than racing it; logins for the same server in two pipelines run side by side. An unfinished one lets go after ten minutes; a daemon restarted mid-login forgets it, and you run the command again.
- **`auth.client_secret_env:` is read from the daemon's environment**, not yours — the daemon is the client.
- **`STEPS_ENCRYPTION_KEY` is the daemon's**, set where its other secrets are (a `fly secrets set`, a systemd credential), never beside the database: a volume snapshot holding both is a snapshot holding the tokens. A daemon started without it still serves every pipeline; it refuses new logins, and a login sealed earlier reads as unreadable until the key is back.
- **`$BROWSER`** names the program either login opens the URL with, as it does for `xdg-open` and `gh`.

> **What this means for the daemon.** It now holds refresh tokens for third-party services, on whatever address it listens on — sealed at rest, but usable by the process that holds the key. `steps pipeline set` already makes that machine one that runs arbitrary commands, so it was never less than fully trusted — but a token for your issue tracker is a way *off* the box that command execution on it is not. Generated credentials and TLS ([authentication.md](authentication.md)) stop being good practice and become the thing standing in front of those tokens.

### Authorizing from the browser: the **mcp** tab

A daemon's web UI has an **mcp** tab for every pipeline that declares `mcp_servers:` — the same inventory `steps mcp list` prints, and, on an oauth server, a **Connect** button that finishes the login in the browser you are already looking at.

It is a strictly shorter path than the CLI rather than a second one. `steps mcp login --target` exists to get an authorization URL out of a daemon and into a browser that can reach that daemon: it starts the login over HTTP, polls until the daemon produces the URL, prints it, and shells out to `$BROWSER`. On the daemon's own page that problem is already solved — you are in a browser, talking to the daemon, authenticated — so what is left is a redirect:

1. **Connect** posts to the daemon, which starts the same login `steps mcp login` starts. The redirect URI is built from your browser's own `Origin`, so a daemon behind a TLS proxy registers the `https://` address you actually reached it on.
2. The daemon waits out discovery and registration (up to 10 seconds) and answers `303` straight to the provider's consent screen.
3. The provider redirects back to `GET /mcp/callback`, which saves nothing you can see and sends you back to the tab.
4. The **token exchange finishes after that redirect**, so the tab is what reports the outcome — including a login that authorized and then failed. The page reads the same status the CLI polls.

**A red run links here.** A job that died on an mcp server — an unauthorized token, a server that did not answer — carries a line under its job error naming that server and linking straight at its row on this tab. That is the direction the question is actually asked from: nobody opens the mcp tab speculatively, they get a red run and want to know why the agent's tools failed. The line appears only for a failure that names a server, and only while the pipeline still declares one.

The tab leads with what cannot be used. Servers needing attention — a missing credential, a login in flight, a server that did not answer — sort above the rest with a count, each keeping the file's own order inside its group. One server is one row: status first, what the server IS on a dimmed second line (`http · oauth · mcp.linear.app/mcp`, full endpoint on hover), and the action in the row it acts on.

The status column answers "is this pipeline's tooling wired up?" without running a job, and it is **never probed on page load**: it comes from the configuration, this machine's environment and `PATH`, and the saved login. A per-server **Test** button is the only thing that connects to anything, and it runs detached — the click returns at once, the row says `testing…`, and the result arrives on the page's own refresh, carrying the tool names the server answered with.

**Test is offered only where a connection could succeed**, and the daemon refuses one that could not, the same way `steps mcp list` declines to dial a server whose credential is already missing. A probe of an unauthorized server answers with the problem the status cell has just stated, in the words of whatever refused it — one problem in two vocabularies, the second long enough to set the width of the page. **A probe that fails outranks the static answer**: the static check only ever said a request *could* be made, and the probe is the request, so the row reads `✗ did not answer` rather than keeping a tick beside a red failure.

Three things worth knowing:

- **A token is saved per pipeline** ([per pipeline](#authorizing-an-oauth-server-steps-mcp-login)), so two pipelines naming one server each need their own login, and each tab shows only its own. A token recorded for a different `endpoint:` reads as `authorized for a different endpoint — needs login` rather than as connected, which is the same refusal a run gives.
- **`--read-only` keeps the page and withholds the buttons.** Connect authorizes and Test dials, which is what that flag is a statement about; `needs login` is precisely the diagnostic a read-only build box should still show, and the row names the CLI command instead.
- **Two people can click Connect on one pipeline's server.** The second login replaces the first, because one saved login cannot be written by two flows. The first person's consent screen still works: their redirect is recognized and sends them back to the tab, which shows the login that won.

A missing bearer variable or a stdio command that is not installed is refused by `steps pipeline set` before a daemon will serve the pipeline at all (see [Preflight checks this before anything runs](#preflight-checks-this-before-anything-runs)), so on a served pipeline those cells normally read ✓ — they turn ✗ when the machine changes underneath a running daemon.

### Servers without dynamic client registration

The flow above discovers the authorization server, then *registers* a client with it on the fly. Some servers don't offer that — Slack's, for one, requires every client to be backed by a pre-registered app with a fixed ID. Against those, login fails during discovery, before your browser ever opens:

```
steps: error: mcp server "slack": authorization server does not support dynamic
  client registration; register an application with it and set auth.client_id
  (plus auth.client_secret_env if it issued a secret)
```

Register the application yourself, then name its credentials:

```yaml noexec=credentials
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth:
    type: oauth
    client_id: "1234567890.9876543210"   # public app identifier
    client_secret_env: SLACK_CLIENT_SECRET
    callback_port: 3118                  # register http://127.0.0.1:3118/callback
                                         # (and https://<daemon>/mcp/callback, to
                                         # authorize a daemon — callback_port is
                                         # the loopback login's alone)

agents:
- name: responder
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: slack

jobs:
- name: respond
  plan:
  - agent: responder
    messages:
      - "Summarize today's mentions."
```

- **`client_id:` skips registration; everything else is identical.** PKCE, the loopback callback, the code exchange, the sealed login, and silent refresh at run/watch time all behave exactly as they do for a dynamically-registered client.
- **`callback_port:` pins the redirect URI, and these servers usually need it.** By default `steps mcp login` listens on an ephemeral port, which is what [RFC 8252 §7.3](https://www.rfc-editor.org/rfc/rfc8252#section-7.3) asks servers to allow for native apps. The MCP authorization spec requires the opposite — authorization servers *must* validate redirect URIs against pre-registered values exactly — and the providers that make you pre-register a client are generally the ones enforcing it. A port that changes every run can never match what you typed into a dashboard, so the login fails with a redirect URI mismatch no matter how `client_id:` is set. Pin the port, register `http://127.0.0.1:<port>/callback` once, and the two agree. It's `oauth`-only, and must be between 1024 and 65535.
- **The ID is in the YAML, the secret is not.** A client ID is a public application identifier, like an endpoint — so it's written literally and its *value* is folded into the cache hash, because pointing a server at a different registered app changes who the calls are made as. The secret follows the `api_key_env:` convention: `client_secret_env:` names an environment variable, and only the name is ever hashed or stored.
- **`client_secret_env:` alone is a load error.** A secret with no ID names a credential for a client that would still be dynamically registered — configured-looking, bound to nothing. Both fields are `oauth`-only; setting either under `bearer`/`none` is rejected at load.
- **A named-but-unset secret variable fails the login**, saying which variable — rather than attempting a public-client exchange the server answers with an opaque 401.
- **Omit both for any server that does support registration.** This is an escape hatch, not the normal path.
