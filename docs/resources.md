# Resources

A **resource** is something outside the pipeline that has versions: a git branch, a queue of pull requests, a build artifact, a counter in a file. A **resource type** is the code that knows how to talk to one.

```yaml
resources:
- name: repo          # the artifact directory a get creates, and the name steps use
  type: git           # which resource type
  source:             # configuration for that type — free-form, type-specific
    uri: https://github.com/jtarchie/ci.git
    branch: main

jobs:
- name: build
  plan:
  - get: repo         # fetch it — the contents land in ./repo
  - task: compile
    run: cd repo && go build ./...
```

`git` ships with steps, so that pipeline needs no `resource_types:` block. For anything else, you write the type yourself.

## The built-in `git` type

| `source:` field | required | meaning |
|---|---|---|
| `uri` | yes | anything `git` can clone — https, ssh, or a local path |
| `branch` | no | omitted follows the remote's `HEAD` |

It fetches the exact commit the plan pinned, shallowly, so a branch that moves mid-run still gives you the version that was planned. It has **no `out:`** — `put: repo` against it is a load error, because what "publish" means (which branch, which credentials, force or not) is a decision only you can make. Write your own type for that.

## Writing a resource type

A resource type is three shell commands. Each is a [template](templating.md) and each runs `sh -c`.

```yaml
resource_types:
- name: counter
  # image: alpine:3          # optional — run these in a container instead of on the host
  config:
    check: |
      printf '[{"ref": "%s"}]' "$(cat {{ .source.path }})"
    in: |
      echo {{ .version.ref | shellquote }} > ./ref
    out: |
      next=$(( $(cat {{ .source.path }}) + 1 ))
      echo "$next" > {{ .source.path }}
      printf '{"ref": "%s"}' "$next"
```

### `check` — what versions exist?

Runs when a plan is built, and on every `steps watch` poll.

- **Sees**: `{{ .source }}`.
- **Must print**: a JSON **array** of version objects to stdout, **oldest first**. A version object is a flat map of strings — `{"ref": "abc123"}`, `{"number": "87"}`. The whole object identifies the version; steps never interprets the fields.
- **Empty array** means "no versions yet". Under `version: every` the get fans out zero times and the job exits 0, so steps prints `get: <name> returned no versions; the N step(s) after it did not run` to say how much of the plan that dropped; any other version mode fails the step with `no versions available`. The message tells you a check came back empty — it cannot tell you *why*, so a type should still **fail loudly** (exit non-zero) when it can't answer, rather than printing nothing.
- **Exit non-zero** to fail the step.

```json
[{"ref": "9fceb02"}, {"ref": "d7b22a6"}]
```

### `in` — fetch one version

Runs when a `get` step executes.

- **Sees**: `{{ .source }}` and `{{ .version }}` (one object from `check`'s array).
- **Working directory** is the artifact directory itself, already created and empty. Write the contents there — `.`, not a subdirectory named after the resource.
- **Exit non-zero** to fail the step.

The fetched directory is named after the `get:`, so `get: repo` puts it in `repo/`, and later steps read `repo/...`. See [workspace.md](workspace.md) for what a step can and can't see.

### `out` — publish something

Runs when a `put` step executes. Optional: a type with no `out:` is read-only, and a `put:` against it is rejected at load time rather than silently doing nothing.

- **Sees**: `{{ .source }}` and `{{ .params }}` (the put step's `params:`).
- **Working directory** is the put step's read view, composed from its `inputs:`.
- **May print** a single JSON **object** — the version it produced. Printing nothing is fine and not an error.

## Shell safety

Anything interpolated into a command is text substitution, so quote it:

```yaml
check: git ls-remote {{ .source.uri | shellquote }}     # good
check: git ls-remote {{ .source.uri }}                  # a uri with a space or ; breaks or worse
```

`shellquote` renders a value as one safely-quoted shell word. Use it for every `{{ }}` that reaches a command. See [templating.md](templating.md).

Templates render with `missingkey=error`, so reading an optional field that wasn't set fails the render. Ask for optional fields in a way that can answer "nothing":

```yaml
{{ index .source "branch" | default "HEAD" }}     # optional
{{ .source.uri }}                                 # required — failing is correct
```

## `version:` on a get step

```yaml
- get: repo                    # default: the latest version
- get: repo
  version: every               # run the rest of the plan once per version
- get: repo
  version: { ref: 9fceb02 }    # pin to exactly this one
```

`every` is the only fan-out point in a plan. A failing version does not stop the remaining ones from being attempted.

## `trigger: true`

Marks a `get` as something `steps watch` should poll. When its version changes, the jobs containing it run automatically. Valid only on `get` steps. See [infra.md](infra.md).

## MCP-backed types

A resource type can call an MCP server instead of running shell commands — the same check/in/out roles, as tool calls. See [mcp.md](mcp.md).

## Checking your work

```bash
steps validate pipeline.yml   # does it parse and hang together?
steps plan pipeline.yml       # runs check; shows what would be fetched vs cached
```

`steps plan` is the fastest way to see whether a `check` you just wrote returns what you expect, since it resolves versions without running the rest of the job.
