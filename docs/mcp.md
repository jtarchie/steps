# MCP Servers

MCP (Model Context Protocol) servers are a third kind of external system, alongside LLM providers (`agents:`) and shell-backed resource types (`resource_types:`): a reusable, named connection (`mcp_servers:`) that an agent's `tools:` grant can draw tools from, and/or a resource type's `check`/`in`/`out` can call instead of shelling out.

**Two transports**: Streamable HTTP (`endpoint:`) or a local subprocess over stdio (`command:`). The examples on this page validate but are not executed by the docs suite — they need real servers and credentials.

## Declaring a server

```yaml noexec
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
    prompt: "Find open issues labeled 'crash' and summarize them."
```

- `name` is how `agents:` tool grants and `resource_types:` `mcp:` blocks reference this server — declared once, shared by any number of consumers, the same "reusable top-level block" idiom as `agents:`/`resources:`.
- `endpoint` must not embed userinfo (`https://user:token@host/`), since it's folded into cache-hashed content — use `auth.api_key_env` for a credential, never the endpoint itself. Checked at load.
- `auth.type` is `"none"` (default, when `auth:` is omitted entirely), `"bearer"`, or `"oauth"`. `"bearer"` requires `api_key_env` — a static token read from that OS environment variable at run time, exactly like an LLM `agents:` entry's `api_key_env` (the value is never stored in YAML or hashed; only the env var *name* is).

## Local (stdio) servers

```yaml noexec
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
    prompt: "Find unused exported functions."
```

A `command:` server is a local subprocess `steps` spawns and speaks newline-delimited JSON to over stdin/stdout, instead of connecting over HTTP.

- **Exactly one of `endpoint:`/`command:`.** Setting both, or neither, is a load error. `args:`/`cwd:` are only valid alongside `command:`.
- **`command:`/`args:` is explicit argv, never a shell.** `command: gopls mcp` is wrong — the whole string is looked up as one executable name; use `args: [mcp]`. There is no globbing, piping, or `&&`.
- **`cwd:` is relative to the agent step's working directory, or absolute.** An **absolute** path is used verbatim. A **relative** path is resolved against the working directory of the agent step whose tools are being built, which is what lets a server be pointed at an input artifact: `cwd: repo` aims a language server at the same materialized tree the agent's own file tools read and edit. **Unset** inherits the directory `steps` was invoked from.

  A relative `cwd:` is rejected at load time for a server backing a **resource type's** `mcp:` config: a `check`/`in`/`out` has no agent step to resolve against. Those need an absolute `cwd:`.
- **Auth is `none` only.** A stdio server has no HTTP request to attach a bearer token to; `auth:` set to anything else alongside `command:` is a load-time error, and `steps mcp login` never applies.
- **Environment is filtered, not inherited.** The subprocess sees only the same host-command allowlist every other host-executed command gets (`PATH`, `HOME`, locale, `TMPDIR`/`USER`/`SHELL`, proxy vars) — **not** the operator's full environment, and specifically not any configured agent's `api_key_env` secret, nor `SSH_AUTH_SOCK`. There is currently no pass-through mechanism for stdio servers, so **a stdio server that needs a credential or any other ambient variable (e.g. `GOFLAGS`, `GOPRIVATE`) is not supported yet**.
- **Lifecycle**: for an agent tool grant, one subprocess is spawned per agent step per grant and reaped when the step ends. For a resource-type `mcp:` backend, a fresh subprocess is spawned **per** `check`/`in`/`out` call — including every `steps watch` poll — fine for a fast-starting binary, a poor fit for one that's slow to start.
- **Diagnostics**: the subprocess's stderr is logged at debug level (`--log-level=debug`), under `mcp.stdio.stderr` with the server's name attached — the only way to see why a stdio server failed to start.

## Discovering a server's tools: `steps mcp tools`

Before writing a tool reference or a resource type's `mcp:` block, find out what a server actually exposes — its tool names and argument schemas are not something to guess:

```bash
steps mcp tools pipeline.yml github
```

This connects (per the server's configured auth) and prints each tool's name, description, and argument schema. It works for any auth type — stdio included — and doubles as a connectivity/auth smoke test.

## Granting MCP tools to an agent

A `tools:` entry can reference an MCP server in three progressively broader forms, all sharing one connection to the server:

```yaml noexec
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
    prompt: "Triage today's crash reports."
```

- **Single tool** (`tool:`): the only form that may also set `description:` (overriding the server's own), `required:`, or `max_calls:` — the same semantics as a custom tool (see [agents.md](agents.md)). Its model-facing function name is `<server>__<tool>` (double underscore — a dot is rejected by OpenAI's function-name charset).
- **Named subset** (`tools:`): a list of tool names, each exposed under its own `<server>__<tool>` name. `description:`/`required:`/`max_calls:` are load-time errors here — they're single-tool concepts.
- **Bare form** (`- mcp: github`, neither set): every tool the server currently exposes.
- **`args:` is invalid on every MCP form** — an MCP tool's arguments are schema-shaped by the remote server, so there's nothing to pin the way a custom tool's `run:` template arguments can be.
- **Grant, not inline**: like a sub-agent tool, an MCP grant must live on the `agents:` entry (or a `fix:`'s own `tools:` override) and be selected by bare name (`tools: [github]`) on a step — a step cannot introduce `{mcp: ..., tool: ...}` inline.
- **Cache hashing**: a server's endpoint/auth type/`api_key_env` name (never a value) — or, for a stdio server, its `command`/`args`/`cwd` — and the granted tool name(s) fold into the step's hash. The bare "all tools" form hashes as a static marker, since plan-time hashing has no live connection to enumerate the server's tools; use the explicit `tools: [...]` form if you want the server's tool list changing to bust the cache.
- **Tool results**: translated to the same map shape every other tool returns — a transport failure or a result with `isError: true` becomes `{"error": ...}`; a successful call becomes `{"structured_content": ..., "content": ...}`.

## Backing a resource type with MCP

```yaml noexec
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
        tool: list_issues    # called with {source}; must return an oldest-first
                             # JSON array of version objects
      out:
        tool: create_issue   # optional — enables `put`; called with {source, params}

resources:
- name: eng-bugs
  type: linear-issues
  source:                    # passed directly as list_issues' arguments
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
```

- **`check:` is required**, `in:`/`out:` are both optional. `mcp:` is mutually exclusive with the shell `check:`/`in:`/`out:` strings — a resource type sets one style or the other.
- **`check`** is called with `{"source": source}`. Its result must be an oldest-first JSON array of version objects, accepted either as structured content or a single text block containing that array — as close a mirror of the shell path's "stdout is a JSON array" convention as an RPC result allows.
- **`in`, when omitted** (the common case — detecting new issues, not fetching their content): `get` just writes the selected version object to `<resource>/version.json`, no MCP call. **When set**, `in`'s tool is called with `{"source", "version"}` and its result is materialized into the get's directory: structured content as `result.json`, each content block as `content-N.<ext>`. `version.json` is always written either way.
- **`out`, when set**, is called with `{"source", "params"}` — `params:` on the `put` step carries the payload. A `put` targeting an MCP-backed type with no `out:` is a load-time error naming the missing tool.
- **Detect vs. respond**: a resource-type `check` feeding `trigger:` is the natural fit for *detecting* something (new issues, a new PR). An open-ended *response* — read context, decide, comment — is usually better as an agent step granted the same server's tools, where the model drives; a deterministic `out:` put (exact fields, no judgment) is the resource-type path.

## Authorizing an oauth server: `steps mcp login`

A bearer-configured server needs no login step — it reads its token from the environment at run time. An oauth-configured server needs a one-time interactive authorization:

```bash
steps mcp login pipeline.yml linear
```

This runs the OAuth 2.1 authorization-code + PKCE flow: discovers the server's metadata, dynamically registers a client, opens your browser (falling back to printing the URL — useful over SSH), and prints where the token was saved.

- **Per-user, not per-pipeline**: the token lands in `${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<server-name>.json` (`0600`) — deliberately outside any pipeline's `.steps/`. Logging in once authorizes that server for **every** pipeline referencing it by the same name.
- **Silent refresh, no re-prompting**: `steps run`/`steps watch` never run an interactive flow — they load the persisted token and refresh it silently. If it can't be refreshed, the error names the exact `steps mcp login` command to run.
- **Trust boundary**: nothing token-shaped is ever cache-hashed or written to `.steps/state.db` — the same treatment as LLM provider credentials.
