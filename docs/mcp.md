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
```

- **Every stage is optional, but a block must do something.** A type declaring none of `check:`/`in:`/`out:` is a load error. A `get` against a type with no `check:` is one too, and so is a `put` against a type with no `out:` — which is what makes a **publish-only type** (all `out:`, no `check:`) a first-class shape: the half of a workflow that posts a reply has no versions to discover, and naming a check tool nothing ever calls would be a ritual. `mcp:` is mutually exclusive with the shell `check:`/`in:`/`out:` strings — a resource type sets one style or the other, which is how a pipeline **detects over HTTP and acts over MCP**: two resource types, one shell-backed and one MCP-backed.
- **The arguments are the remote tool's own schema.** A tool publishes the parameters it takes and rejects a call that omits a required one, so steps sends exactly what you name and wraps it in nothing: `check` and `in` send the resource's `source:`, `out` sends the put's `params:`. Run `steps mcp tools <pipeline> <server>` to see what a tool actually requires before writing either.
- **`check`**'s result must be an oldest-first JSON array of version objects, accepted either as structured content or a single text block containing that array — as close a mirror of the shell path's "stdout is a JSON array" convention as an RPC result allows.
- **`in`, when omitted** (the common case — detecting new issues, not fetching their content): `get` just writes the selected version object to `<resource>/version.json`, no MCP call. **When set**, its result is materialized into the get's directory: structured content as `result.json`, each content block as `content-N.<ext>`. `version.json` is always written either way.
- **`out`, when set**, is what a `put` calls; `params:` on the step carries the payload. A `put` targeting an MCP-backed type with no `out:` is a load-time error naming the missing tool.

### Naming the arguments: `args:`

`source:` and `params:` only work as arguments when their keys already match the tool's parameters. When they don't — or when the value the tool needs lives on the **version** a check produced, which is the usual shape for `in:` — name the mapping instead. Every string in it is a template over exactly what that stage has, the same as a shell `check`/`in`/`out` command: `check` renders against `{source}`, `in` against `{source, version, params}`, `out` against `{source, params}`.

```yaml noexec
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
```

- **`args:` replaces the default payload entirely** — it is the argument object, not an addition to it.
- **Non-string values pass through untouched**, so `limit: 20` reaches the tool as the number 20. Templates nest through mappings and lists.
- **A template naming a field that isn't there fails the step**, exactly as a shell command's template does — a typo'd `{{ .version.chanel }}` is not quietly nothing. That includes naming a stage's missing half: `{{ .version.x }}` in `check.args:` fails, because a check has no version yet.
- **A templated value is always a string.** `limit: 20` stays the number 20, but `limit: "{{ .source.limit }}"` sends `"20"`. Numbers lifted out of a version keep their exact digits — an issue id of `123456789` renders as `123456789`, not in exponent form.
- **`{file: ...}` markers in a `put`'s `params:` are resolved first**, so `{{ .params.text }}` renders the file's contents.

#### Upgrading a resource type written before `args:`

steps used to wrap every call in an envelope of its own: `check` was called with `{"source": source}`, `in` with `{"source": source, "version": version}`, and `out` with `{"source": source, "params": params}`. No third-party server has ever declared parameters by those names, which is why an off-the-shelf tool could not be called at all — but a server written *for* steps could read them, and those are the ones this changes:

| stage | was called with | now called with (no `args:`) |
|---|---|---|
| `check` | `{source}` | the source itself |
| `in` | `{source, version}` | the source itself — **the version is gone** |
| `out` | `{source, params}` | the params themselves — **the source is gone** |

An `in:` reading `arguments.version.id` now receives the source and no version, and a tool whose parameters are all optional will accept that and quietly do the wrong thing. Name what the tool takes instead:

```yaml fragment
in:
  tool: get_issue
  args:
    issue_id: "{{ .version.id }}"
```

The old envelope cannot be restored verbatim — a template renders a string, so there is no `{{ .source }}` that emits the whole object; enumerate the fields the tool needs (`args: { source: { team: "{{ .source.team }}" } }`) or, better, give the tool real parameters. `args:` is part of a step's hash, so adding a mapping re-runs the affected `get`/`put` rather than reusing what the old envelope fetched.

### When a tool returns prose: detect over HTTP, act over MCP

A `check` needs a machine-readable list, and not every MCP tool has one to give. Many vendor servers answer with Markdown written for a model to read — Slack's search returns `{"results": "# Search Results for: …", …}`, with no `structuredContent` and no published `outputSchema`. There is no list in there to select, at any nesting depth, so no mapping can make that tool a `check`. (This is not a niche accident: until [SEP-2106](https://modelcontextprotocol.io/specification/) `structuredContent` was required to be a JSON *object*, so an array-returning tool was not even legal.)

The split that does work: let the vendor's HTTP API do the **detecting**, where the response is JSON with stable ids, and let MCP do the **acting**, where prose in the response costs nothing. Two resource types, one of each style:

```yaml noexec
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth: { type: oauth }

resource_types:
- name: slack-mentions          # DETECT: real JSON, stable {channel, ts}
  env: [SLACK_BOT_TOKEN]
  config:
    check: |
      curl -sS -H "Authorization: Bearer $SLACK_BOT_TOKEN" \
        "https://slack.com/api/conversations.history?channel={{ .source.channel }}&limit=20" |
      jq -c '[.messages[] | {channel: "{{ .source.channel }}", ts: .ts}] | reverse'

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

Both ways an MCP call is wrong — a tool the server doesn't expose, and required arguments the call will never send — are answerable from the server's published tool list, without calling anything. So they are: `steps run` checks the resources its job touches before the first step, `steps preflight` asks the same question on demand, and **`steps watch` checks every `trigger:` resource before its first poll and exits if one can't work**. That last one is the point: a poll loop's reaction to a permanent misconfiguration is to log it and try again on the next interval, forever, with nothing enqueued and nothing red.

```
watch: preflight failed, nothing was polled:
  resource "mentions": check tool "slack_search_public_and_private" requires [query], which this call does not send (it sends: [to])
    (the resource's source: IS the argument object when mcp.check.args: is unset — name it there, or map it in mcp.check.args:)
```

`--no-preflight` skips it, as it does for models.


### Sending a file's contents: `{file: ...}` in `params:`

A shell `out:` runs with the put's read view as its working directory and reads what it needs. An MCP `out:` is a tool call with no working directory, so a value a *previous step wrote* needs a way in — otherwise the payload could only ever be text the pipeline author typed, which rules out publishing anything an agent produced.

A `params:` mapping whose **only** key is `file` is replaced by that file's contents:

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
  - put: eng-bugs
    inputs: [report]
    params:
      title: Retry loop spins
      description: { file: report/body.md }   # <- contents, not the literal map
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
steps mcp login pipeline.yml linear
```

This runs the OAuth 2.1 authorization-code + PKCE flow: discovers the server's metadata, dynamically registers a client, opens your browser, and prints where the token was saved.

- **The authorization URL is always printed**, whether or not the browser opened:

  ```
  Authorize in your browser:

    https://auth.example.com/authorize?client_id=…&code_challenge=…
  ```

  A browser that opens the wrong profile, opens nothing visible, or isn't there at all (SSH, a container) is indistinguishable from success on this side — the flow would just sit and wait with nothing on screen. Copy the URL into any browser that can reach the loopback address printed in `redirect_uri` and the login completes normally.
- **A slightly malformed `WWW-Authenticate` challenge is tolerated.** Some servers separate the challenge's parameters with spaces where the HTTP spec wants commas (Metabase's MCP endpoint does). The header still says exactly one thing, so steps normalizes it and continues rather than failing the login over punctuation.
- **An unadvertised `iss` is tolerated, a wrong one is not.** RFC 9207 has the authorization server return its own identifier on the redirect, and servers are supposed to declare that in their metadata. Some return it without declaring it (Metabase again). steps checks the value against the issuer it discovered: matching is fine and the login proceeds; an `iss` naming a *different* issuer is the mix-up attack the parameter exists to catch, and fails the login by name.
- **Per-user, not per-pipeline**: the token lands in `${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<server-name>.json` (`0600`) — deliberately outside any pipeline's `.steps/`. Logging in once authorizes that server for **every** pipeline referencing it by the same name.
- **Silent refresh, no re-prompting**: `steps run`/`steps watch` never run an interactive flow — they load the persisted token and refresh it silently. If it can't be refreshed, the error names the exact `steps mcp login` command to run.
- **The refresh grant is registered, not assumed.** Dynamic registration declares `grant_types: [authorization_code, refresh_token]`, because RFC 7591 defaults an omitted `grant_types` to `authorization_code` *alone* — and a conforming server then issues an access token with no refresh token beside it, exactly as asked. That looks like a successful login and dies at the first expiry, unattended.
- **A login that can't be renewed is a failed login.** If the authorization server returns no refresh token for a token that *does* expire, `steps mcp login` saves it (it works right now) and exits non-zero saying when it stops working:

  ```
  steps: error: mcp login: mcp server "metabase": authorized, and the token was saved —
    but the authorization server issued no refresh token, so this credential stops working
    at 2026-08-13T22:44:05-06:00 and `steps run`/`steps watch` will fail from then on with
    no way to renew it.
  ```

  A token with no expiry at all is fine and reports nothing: there is nothing to renew.
- **`auth.scopes:` is what gets requested.** Left unset, steps asks for every scope the server's protected-resource metadata advertises, which is the right default for a server the pipeline has no opinion about. Setting it narrows the authorization request to exactly those scopes — worth doing against a server whose scope list includes writes you never intend to make.
- **Trust boundary**: nothing token-shaped is ever cache-hashed or written to `.steps/state.db` — the same treatment as LLM provider credentials.

### Servers without dynamic client registration

The flow above discovers the authorization server, then *registers* a client with it on the fly. Some servers don't offer that — Slack's, for one, requires every client to be backed by a pre-registered app with a fixed ID. Against those, login fails during discovery, before your browser ever opens:

```
steps: error: mcp server "slack": authorization server does not support dynamic
  client registration; register an application with it and set auth.client_id
  (plus auth.client_secret_env if it issued a secret)
```

Register the application yourself, then name its credentials:

```yaml noexec
mcp_servers:
- name: slack
  endpoint: https://mcp.slack.com/mcp
  auth:
    type: oauth
    client_id: "1234567890.9876543210"   # public app identifier
    client_secret_env: SLACK_CLIENT_SECRET

agents:
- name: responder
  source: { model: openrouter/qwen/qwen3.7-flash, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: slack

jobs:
- name: respond
  plan:
  - agent: responder
    prompt: "Summarize today's mentions."
```

- **`client_id:` skips registration; everything else is identical.** PKCE, the loopback callback, the code exchange, the token file, and silent refresh at run/watch time all behave exactly as they do for a dynamically-registered client.
- **The ID is in the YAML, the secret is not.** A client ID is a public application identifier, like an endpoint — so it's written literally and its *value* is folded into the cache hash, because pointing a server at a different registered app changes who the calls are made as. The secret follows the `api_key_env:` convention: `client_secret_env:` names an environment variable, and only the name is ever hashed or stored.
- **`client_secret_env:` alone is a load error.** A secret with no ID names a credential for a client that would still be dynamically registered — configured-looking, bound to nothing. Both fields are `oauth`-only; setting either under `bearer`/`none` is rejected at load.
- **A named-but-unset secret variable fails the login**, saying which variable — rather than attempting a public-client exchange the server answers with an opaque 401.
- **Omit both for any server that does support registration.** This is an escape hatch, not the normal path.
