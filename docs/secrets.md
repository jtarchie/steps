# Secrets

A pipeline that needs a credential asks for it by a short name, and the
`steps web` daemon fetches it when the step that needs it runs. Steps never
stores the value: the state database records the reference, not what it
resolved to.

```yaml noexec=secrets
tasks:
- name: publish
  secrets:
    NPM_TOKEN: ((ci:npm_token))
  run: npm publish

jobs:
- name: release
  plan:
  - task: publish
  - task: announce
    secrets:
      GH_TOKEN: ((ci:gh_token))
    run: gh release create v1.0.0 --notes "published"
```

Terms:

- **A secret** is a credential a pipeline needs and must never store.
- **A var** (`((repo_uri))`, no colon) is a parameter, not a secret. Its value
  is hashed and written to the state database, which is why a credential cannot
  be one (see [templating.md](templating.md)).
- **A source** (`ci` above) is a named place secrets come from. The admin who
  runs `steps web` defines it; a pipeline cannot.
- **A lookup path** turns a pipeline's short name into the full location in the
  secret store. Paths are tried in order and the first one found wins.

An agent takes the same map. Its `run_shell` and custom tools see the
variables; the model does not, because every tool result is scrubbed before it
becomes the next message:

```yaml noexec=secrets
agents:
- name: triage
  source:
    endpoint: https://openrouter.ai/api/v1/
    model: anthropic/claude-sonnet-5.5
    api_key_env: OPENROUTER_API_KEY
  tools: [run_shell]
  secrets:
    GH_TOKEN: ((ci:gh_token))

jobs:
- name: triage
  plan:
  - agent: triage
    messages: [Label the open issues with gh.]
```

A resource takes it too. Its `check`, `in` and `out` see the variables, an
`expr:` type's `env()` reads them by name, and a native type's `token_env:` may
name one:

```yaml noexec=secrets
resources:
- name: pr
  type: github-prs
  source:
    repo: acme/app
    token_env: GH_TOKEN
  secrets:
    GH_TOKEN: ((ci:gh_token))

jobs:
- name: review
  plan:
  - get: pr
    trigger: true
```

## Where a reference may appear

Only in a `secrets:` map, on a `tasks:` entry, an `agents:` entry, a
`resources:` entry, or a task or agent step. A get or put step has no
`secrets:` of its own; its resource's apply. Each key is the
environment variable the step's commands see; each value is exactly one
`((source:name))`. A step's own `secrets:` replaces its task's, the way `env:`
does. A name may not be in both `env:` and `secrets:`.

A reference anywhere else is a load error: written into `run:` it would be
hashed, stored and put on a command line. That includes a resource's
`source:`: name the variable in `secrets:` and read it from the environment, as
`"$GH_TOKEN"` in a shell command or `env("GH_TOKEN")` in an expression.

`secrets:` is refused on a CLI agent (`source: cli:…`), because the CLI runs its
own tools and shows their output to its own model, which steps cannot scrub. It
is refused on an agent used as a `fix:` agent or a sub-agent too: only an agent
step resolves secrets, and a silently missing variable is worse than a load
error. An agent's `api_key_env:` still names a variable in the daemon's own
environment, not a `secrets:` key.

A bare `((x))` is always a var, never a secret. Whether a reference is a secret
is decided by its text alone, before anything is fetched. Concourse differs here:
an unset `((x))` there quietly becomes a credential lookup.

Names are letters, digits and `_ . / -`, with no empty, `.` or `..` segment, so
`((ci:../prod/db))` cannot climb out of the folder a lookup path puts it in.
`steps pipeline set -v` substitutes vars before the pipeline is read, so a var
can introduce a reference.

## Defining sources

Sources live in the `secret:` section of the daemon's
[config file](web.md#configuration-file), nested under `web:`:

```yaml config
web:
  local-users-file: /etc/steps/users
  secret:
    source:
      ci:
        command: op read --no-newline {{ .path | shellquote }}
        missing: exit_code == 1 && stderr contains "isn't an item"
        timeout: 5m
        lookup:
        - path: op://ci/{{ .pipeline }}/{{ .name }}
        - path: op://ci/release/{{ .name }}
          when: pipeline in ["deploy", "release"]
        - path: op://ci/shared/{{ .name }}
```

For the `deploy` pipeline, `((ci:gh_token))` tries `op://ci/deploy/gh_token`,
then `op://ci/release/gh_token`, then `op://ci/shared/gh_token`. For a pipeline
named `docs`, the release path is never tried.

- **`command:`** runs under `sh -c` as the daemon's user, with the daemon's
  whole environment, so `op`, `vault` and `aws` find their own logins. The
  lookup path reaches it as `{{ .path }}`; quote it with `shellquote`. What it
  prints on stdout is the value, with exactly one trailing newline removed. An
  empty value is refused.
- **`missing:`** is an expression over `exit_code` and `stderr` that says "this
  path does not have it". Only that answer moves on to the next path; any other
  failure stops the step. It is required when there is more than one path:
  `op read` exits 1 both for a missing item and for an expired session, and
  reading every failure as missing would quietly run a build with the shared
  credential. Prefer `exit_code` where the tool tells the cases apart (Vault
  does), since stderr text changes with the locale.
- **`lookup:`** paths are templates over `pipeline`, `job`, `resource` and
  `name`. A path with **`when:`**, an expression over the same four, is tried
  only where it is true. Only `pipeline` is chosen by an admin, through
  `steps pipeline set`; the others are the pipeline author's to name, so scope
  rules on `pipeline` and use the rest as refinements.
- **`timeout:`** bounds each attempt (default 2m, longer than any other default
  in steps, because a person may have to approve a Touch ID prompt). The whole
  process group is killed when it expires.

On failure the step reports the source's exit code and stderr, never its stdout.

**A daemon with a secret source requires a login**, loopback included; see
[web.md](web.md#configuration-file). Configure `--role` beside it: with Basic
auth and no roles, every signed-in person is admin and can set any pipeline.

## When a secret is fetched

Lazily, by the step that needs it, after the step's worker is acquired and
before its first command. A branch that never runs fetches nothing. Each run
asks the source once per reference, so a step re-placed after a spot reclaim
reuses the value instead of prompting again. A failed lookup is not remembered,
so a retried attempt asks again.

`steps pipeline set` checks that every source a pipeline names is configured,
and never runs one. `steps run` and `steps test` refuse a pipeline with a
reference, since sources exist only on the daemon; `steps validate` accepts it.

**`steps validate --live --config steps.yml` resolves every reference** the
way the daemon would, given the daemon's own config file, and prints the path
that answered, never the value:

```
secret ((ci:gh_token)) for job "release" is at op://ci/shared/gh_token
secret ((ci:npm_token)) for job "release" is at op://ci/deploy/npm_token
ok: pipeline.yml (1 job(s), 0 resource(s), 0 agent(s)) — every model and MCP server responded, 2 secret(s) found
```

Since resolution is lazy, this is the one way to learn that an `op signin` has
lapsed before a job is half paid for, and how an admin sees a name fall
through to the shared folder. A reference no path has is reported with the
paths tried. `--job` narrows it to that job's references; a polled resource's
is no job's. Run it on the daemon's machine as the daemon's user, since the
source commands run as whoever invokes it.

**A resource polled by `steps web` resolves outside any run.** Its answers are
cached for one `--interval`, so a `trigger: true` resource does not prompt every
poll and a rotated credential reaches the poller within one interval. A check
whose resource has secrets never shares its answer with another pipeline's
identical check, since the same name can resolve to a different credential.

## Where the value goes

Into the step's environment, and nowhere else steps writes:

- **Never hashed.** A task or agent step keys on the reference, so rotating a
  credential re-runs nothing, and neither does editing a lookup path. A
  resource keys on the secret's name only, like `env:`: which credential answers
  decides whether a version can be fetched, not what the version is.
- **Never in a container's configuration.** On a docker-backed worker the value
  is passed with each command, not when the container is created, so it is not
  in `docker inspect` or on the daemon's disk, even though the container is kept
  after the step.
- **Never on a command line.** On a bare `ssh://` worker it travels over the
  session's stdin. A worker never sees a source command or a lookup path, and
  never holds the login that reads the store.

**Scrubbed from what a step prints.** A value the run has resolved is replaced
with `***` in the step's recorded output, in the live stream the browser shows,
in the output an `assert:` checks or a `fix:` agent is shown, in every tool
result an agent's model is sent, and in a failed check's recorded error. A value printed in two pieces is still caught, and each line of a
multi-line value (a PEM key) is masked on its own. A value shorter than 8 bytes
is masked too, with a warning in the daemon's log, since it may also mask
ordinary text.

A sub-agent does not receive its parent step's secrets either.
A `fix:` agent does not receive the task's secrets. Its tools, including its
own rerun of the task, run without them; the verdict re-run after the agent
finishes has them again.

What scrubbing cannot catch: an encoded form of the value (base64, JSON
escaping, a URL-encoded copy), and files a step writes. Outputs, artifacts and
cache directories are stored as written. On Postgres, a value that reached a
query would also reach the server's statement log if that is on.

What steps cannot protect: anything the step itself does with the value, root
or the same user reading a process's memory or environment while the step runs,
swap on the daemon or a worker, and a plain `run:` step on the daemon's own
machine, which runs as the daemon's user and could call `op` itself. Real
isolation between teams needs steps that run in a container, on a worker, or
under `sandbox: true`.
