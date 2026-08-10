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

- **Sees**: `{{ .source }}`, `{{ .version }}` (one object from `check`'s array), and `{{ .params }}` (the get step's `params:`).
- **Working directory** is the artifact directory itself, already created and empty. Write the contents there — `.`, not a subdirectory named after the resource.
- **Exit non-zero** to fail the step.

`params:` on a get is how a resource is told *how* to fetch, as opposed to `source:`, which says *what* to fetch:

```yaml
- get: repo
  params:
    depth: 1
```

The distinction matters because `source:` belongs to the resource and `params:` belongs to the step, so one repository can be fetched shallowly in a job that only needs the tip and fully in a job that walks history — without declaring it twice:

```yaml
resources:
- name: repo
  type: git
  source: { uri: https://github.com/you/repo.git }

jobs:
- name: quick
  plan:
  - get: repo
    params: { depth: 1 }
- name: changelog
  plan:
  - get: repo          # same resource, full history
```

**Optional params take the same shape as an optional `source:` field** (see [Shell safety](#shell-safety) below). Templates render with `missingkey=error`, so a bare `{{ .params.depth }}` makes `depth` *mandatory* on every get of that type:

```yaml
in: git clone --depth {{ index .params "depth" | default "50" }} {{ .source.uri | shellquote }} .
```

That works on an absent key and on a get with no `params:` block at all.

**Params change the fetch, so they change the hash.** Two gets of one version differing in `params:` are two different fetches: they get distinct cache entries and neither is reused for the other. A get with no `params:` hashes exactly as it did before the field existed, so adding this to a pipeline invalidates nothing that does not use it.

The fetched directory is named after the `get:`, so `get: repo` puts it in `repo/`, and later steps read `repo/...`. See [workspace.md](workspace.md) for what a step can and can't see.

### `out` — publish something

Runs when a `put` step executes. Optional: a type with no `out:` is read-only, and a `put:` against it is rejected at load time rather than silently doing nothing.

- **Sees**: `{{ .source }}` and `{{ .params }}` (the put step's `params:`).
- **Working directory** is the put step's read view, composed from its `inputs:`.
- **May print** a single JSON **object** — the version it produced. Printing nothing is fine and not an error.

### The implicit get after a put

When a put succeeds, `steps` immediately fetches the version it produced, so later steps can use it — the artifact is named after the put:

```yaml
- put: release            # out: publishes and prints {"ref": "v1.4.2"}
- task: verify
  inputs: [release]       # ...and here it is, already fetched
  run: cat release/ref
```

This mirrors Concourse. The fetch runs the resource type's `in:` exactly as a `get` step would, so a type needs no extra support for it.

- **`get_params:`** passes params to that fetch, the way a get step's own `params:` do:
  ```yaml
  - put: image
    get_params: { skip_download: true }
  ```
- **`no_get: true`** skips it, for a put at the end of a plan whose output nothing reads. The artifact is then never produced, and a later step naming it is a validation error rather than a run-time surprise. Setting `get_params:` alongside `no_get:` is a load error — one of the two lines would do nothing.
- **A put whose `out:` printed no version fetches nothing and still succeeds.** There is no version to fetch, and since printing nothing is legal here (unlike Concourse, which expects a version), failing would break every resource type that publishes without versioning what it published.
- **The fetch happens before the step is recorded**, so a put whose implicit get fails is a failed step rather than a green one missing its artifact.

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

Marks a `get` as something `steps watch` should poll. When its version changes, the jobs containing it run automatically. Valid only on `get` steps — setting it anywhere else is a load-time error, demonstrated by [examples/invalid/trigger-on-task.yml](../examples/invalid/trigger-on-task.yml). See [infra.md](infra.md).

## MCP-backed types

A resource type can call an MCP server instead of running shell commands — the same check/in/out roles, as tool calls. See [mcp.md](mcp.md).

## Checking your work

```bash
steps validate pipeline.yml   # does it parse and hang together?
steps plan pipeline.yml       # runs check; shows what would be fetched vs cached
```

`steps plan` is the fastest way to see whether a `check` you just wrote returns what you expect, since it resolves versions without running the rest of the job.
