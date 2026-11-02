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

## Where a reference may appear

Only in a `secrets:` map, on a `tasks:` entry or on a task step. Each key is the
environment variable the step's commands see; each value is exactly one
`((source:name))`. A step's own `secrets:` replaces its task's, the way `env:`
does. A name may not be in both `env:` and `secrets:`.

A reference anywhere else is a load error: written into `run:` it would be
hashed, stored and put on a command line. `secrets:` on agent steps and secret
values in a resource's `source:` are not supported yet and are refused.

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

## Where the value goes

Into the step's environment, and nowhere else steps writes:

- **Never hashed.** The step cache keys on the reference, so rotating a
  credential re-runs nothing, and neither does editing a lookup path.
- **Never in a container's configuration.** On a docker-backed worker the value
  is passed with each command, not when the container is created, so it is not
  in `docker inspect` or on the daemon's disk, even though the container is kept
  after the step.
- **Never on a command line.** On a bare `ssh://` worker it travels over the
  session's stdin. A worker never sees a source command or a lookup path, and
  never holds the login that reads the store.

**Scrubbed from what a step prints.** A value the run has resolved is replaced
with `***` in the step's recorded output, in the live stream the browser shows,
and in the output an `assert:` checks or a `fix:` agent is shown, so a model
never sees it. A value printed in two pieces is still caught, and each line of a
multi-line value (a PEM key) is masked on its own. A value shorter than 8 bytes
is masked too, with a warning in the daemon's log, since it may also mask
ordinary text.

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
