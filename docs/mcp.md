# MCP Servers

MCP (Model Context Protocol) servers are a third kind of external system, alongside LLM providers (`agents:`) and shell-backed resource types (`resource_types:`): a reusable, named connection (`mcp_servers:`) that an agent's `tools:` grant can draw tools from, and/or a resource type's `check`/`in`/`out` can call instead of shelling out. See `examples/agents.yml`'s `triager` job for the agent-tool-grant half and `examples/infra.yml`'s `notify-linear` job for the resource-type half.

**Scope for v1**: HTTP (Streamable HTTP) transport only — stdio is a documented future extension, not yet supported. A resource type's `out:` is optional and, when set, receives only `{source, params}` — it cannot read step working-directory files the way a shell `out:` can.

## Declaring a server

```yaml
mcp_servers:
- name: github
  endpoint: https://api.githubcopilot.com/mcp/
  auth: { type: bearer, api_key_env: GITHUB_PAT }

- name: linear
  endpoint: https://mcp.linear.app/mcp
  auth: { type: oauth }
```

- `name` is how `agents:` tool grants and `resource_types:` `mcp:` blocks reference this server — declared once, shared by any number of consumers, the same "reusable top-level block" idiom as `agents:`/`resources:`.
- `endpoint` is validated at `LoadConfig` the same way `AgentSource.Endpoint` is (`validateMCPServers`, mirroring `validateAgentEndpoints`): it must not embed userinfo (`https://user:token@host/`), since it's folded into merkle-hashed content — use `auth.api_key_env` for a credential, never the endpoint itself.
- `auth.type` is `"none"` (default, when `auth:` is omitted entirely), `"bearer"`, or `"oauth"`. `"bearer"` requires `api_key_env` — a static token read from that OS environment variable at run time, exactly like an LLM `agents:` entry's `api_key_env` (the value is never stored in YAML or hashed; only the env var *name* is).

## Discovering a server's tools: `steps mcp tools`

Before writing a tool reference or a resource type's `mcp:` block, find out what a server actually exposes — its tool names and argument schemas are not something to guess:

```bash
steps mcp tools examples/agents.yml github
```

This connects (per the server's configured auth) and prints each tool's name, description, and argument schema. It works for any auth type, and doubles as a connectivity/auth smoke test.

## Granting MCP tools to an agent

A `tools:` entry can reference an MCP server in three progressively broader forms, all sharing one connection to the server:

```yaml
agents:
- name: triager
  source: { model: openrouter/anthropic/claude-3.5-sonnet, api_key_env: OPENROUTER_API_KEY }
  tools:
  - mcp: github
    tool: search_issues           # one tool — may also set description/required/max_calls
    max_calls: 5
  - mcp: github
    tools: [get_issue, list_pulls] # a named subset — each keeps its own server-advertised description
  # - mcp: github                  # bare form: every tool the server exposes
```

- **Single tool** (`tool:`): the only form that may also set `description:` (overriding the server's own), `required:`, or `max_calls:` — the same semantics as a custom tool (see [agents.md](agents.md)'s call-guards section). Its model-facing function name is `<server>__<tool>` (double underscore — a dot, which "server.tool" would naturally suggest, is rejected by OpenAI's function-name charset).
- **Named subset** (`tools:`): a list of tool names, each exposed under its own `<server>__<tool>` name with the server's own description. `description:`/`required:`/`max_calls:` are load-time errors here — they're single-tool concepts.
- **Bare form** (neither set): every tool the server currently exposes, discovered via `steps mcp tools` at your own pace — not something `steps` re-checks automatically (see the merkle caveat below).
- **`args:` is invalid on every MCP form** — an MCP tool's arguments are schema-shaped by the remote server, not a flat string template, so there's nothing to pin the way a custom tool's `run:` template arguments can be.
- **Grant, not inline**: like a sub-agent tool, an MCP grant must live on the `agents:` entry (or a `fix:`'s own `tools:` override — MCP tools *are* allowed in a fix agent's grant, unlike sub-agents) and be selected by bare name (`tools: [github]` on a step) — a step cannot introduce `{mcp: ..., tool: ...}` inline. This is enforced at `LoadConfig` and again at the tool-grant merge point.
- **Merkle hashing**: a server's endpoint/auth type/`api_key_env` name (never a value) and the granted tool name(s) fold into the step's hash — editing either busts the cache. The bare "all tools" form hashes as a static marker, since merkle-time planning has no live connection to the server to enumerate its current tools; use the explicit `tools: [...]` form if you want a server's tool list changing to bust the cache on its own.
- **Tool results**: translated to the same map shape every other tool returns — a transport failure or a tool result with `isError: true` becomes `{"error": ...}`; a successful call becomes `{"structured_content": ..., "content": ...}` (the joined text content).

## Backing a resource type with MCP

```yaml
resource_types:
- name: linear-issues
  config:
    mcp:
      server: linear
      check:
        tool: list_issues   # called with {source} as arguments; must return
                             # an oldest-first JSON array of version objects
      # in:                 # optional — omitted here, so get just writes
      #   tool: get_issue    # version.json (see below); no MCP call
      out:
        tool: create_issue  # optional — enables `put`; called with {source, params}

resources:
- name: eng-bugs
  type: linear-issues
  source:                   # passed directly as list_issues' arguments —
    team: ENG                # this is the "criteria config" for detecting new issues
    label: bug
    state: Backlog
```

- **`check:` is required**, `in:`/`out:` are both optional. `mcp:` is mutually exclusive with the shell `check:`/`in:`/`out:` strings — a resource type sets one style or the other, checked at `LoadConfig`.
- **`check`** is called with `{"source": source}`, mirroring the shell backend's own template data shape. Its result must be an oldest-first JSON array of version objects, accepted either as the tool result's structured content or a single text content block containing that same array — as close a mirror of the shell path's "stdout is a JSON array" convention as an RPC result allows.
- **`in`, when omitted** (the common case — the motivating use case here is detecting new issues, not fetching their full content): `get` just writes the selected version object to `<resource>/version.json`. No MCP call. **When set**, `in`'s tool is called with `{"source": source, "version": version}`, and its result is materialized into the get step's directory: structured content (if present) as `result.json`, and each content block as `content-N.<ext>` (text as `.txt`; image/audio by MIME type; anything else as a best-effort `content-N.json`). `version.json` is always written either way, so a job can always rely on it being there.
- **`out`, when set**, is called with `{"source": source, "params": params}` — `params:` on the `put` step carries the payload (e.g. the fields of an issue to create), symmetric with how `check` uses `source:`. Its result is parsed into the produced version object the same way `check` parses one array element. A `put` targeting an MCP-backed type with no `out:` configured is a load-time error, not a silent no-op — it names the missing tool and suggests the agent-tool path (above) as an alternative for a response that needs a model's judgment rather than a fixed payload.
- **Detect vs. respond**: a resource-type `check` polling for new versions and feeding `trigger:` is the natural fit for *detecting* something (new Linear issues, a new PR). An open-ended *response* — read context, decide, comment — is usually a better fit as an agent step granted the same server's tools (above), where the model drives; a deterministic `out:` put (exact fields, no judgment involved) is the resource-type path. Both are legitimate; pick per how much judgment the response needs.

## Authorizing an oauth-configured server: `steps mcp login`

A bearer-configured server (`api_key_env`) needs no login step — it reads its token from the environment at run time. An oauth-configured server needs a one-time interactive authorization:

```bash
steps mcp login examples/infra.yml linear
```

This runs the OAuth 2.1 authorization-code + PKCE flow: discovers the server's protected-resource and authorization-server metadata, dynamically registers a client, opens your browser (falling back to printing the URL if that fails — useful over SSH or on a headless box), and on success prints where the token was saved.

- **Per-user, not per-pipeline**: the resulting token is saved to `${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<server-name>.json` (`0600`, directory `0700`) — deliberately outside any pipeline's own `.steps/` directory. An OAuth token is a per-user-per-service credential, not a per-pipeline execution artifact: logging in once authorizes that server for **every** pipeline that references a server by the same name, and it keeps this file out of the plan-time hashing / `.steps/state.db` call chain entirely (no pipeline path is threaded through it).
- **Silent refresh, no re-prompting**: `steps run`/`steps watch` never run an interactive flow themselves — they load the persisted token and refresh it silently as needed, writing the refreshed (and, for providers that rotate it, the new refresh) token back to the same file. If the token can't be refreshed (revoked, or never logged in), the error names the exact `steps mcp login` command to run.
- **Trust boundary**: like `AgentSource.APIKeyEnv`, nothing token-shaped is ever merkle-hashed or written to `.steps/state.db` — the token file is a separate, non-hashed mechanism, the same treatment this repo's Trust Boundaries documentation (see the root `CLAUDE.md`) already gives LLM provider credentials.

## Command surface

- `steps mcp tools <pipeline> <server>` — list a server's tools and argument schemas. Any auth type.
- `steps mcp login <pipeline> <server>` — interactively authorize an `auth: {type: oauth}` server. The only interactive command in this group; `run`/`watch` never prompt.
