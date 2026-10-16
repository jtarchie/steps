# Template Rendering

Resource `check`/`in`/`out` commands and agent custom tools are [Go templates](https://pkg.go.dev/text/template) rendered before they run: `{{ .source.* }}`, `{{ .version.* }}` and a step's `{{ .params.* }}` for resources, plus `{{ .args.* }}` in a custom tool's `run:` (where the tool's parameter schema is inferred from exactly those references — see [agents.md](agents.md)).

Templates have the [slim-sprig](https://github.com/go-task/slim-sprig) function library (string/list/default/date helpers) plus our own `shellquote`. Its three host-reaching functions are removed — `env`, `expandenv` and `getHostByName` — because a template renders on the machine running the pipeline, which may be a daemon that anyone with API access can upload a pipeline to:

```yaml
resource_types:
- name: shouts
  config:
    check: |
      printf '[{"word": "%s"}]' {{ .source.word | upper | shellquote }}
    in: |
      echo {{ .version.word | shellquote }} > word.txt

resources:
- name: shout
  type: shouts
  source: { word: hello }

jobs:
- name: demo
  plan:
  - get: shout
  - task: show
    inputs: [shout]
    run: cat shout/word.txt
    assert:
      stdout: HELLO                # `upper` ran in the check template, not here
  assert:
    execution: [shout, show]
    outcome: succeeded
```

## Shell-quoting untrusted values

A rendered template runs via `sh -c`, so any interpolated value that could contain shell metacharacters — backticks, `$(...)`, quotes, `; | &` — must be piped through `shellquote`, which renders it as one safely-quoted POSIX word and supplies its own quotes:

```yaml fragment
run: gh pr comment --body {{ .args.body | shellquote }}
```

Don't add surrounding `"..."` — `shellquote` already quotes only when needed. This matters most for LLM- or PR-authored values (e.g. a review body): without it, a body containing `` `replace` `` gets command-substituted by the shell and posted with those words silently missing.

`steps validate` warns — and still exits 0 — on every `{{ }}` that reaches the shell without ending in `shellquote`: in a resource `check`/`in`/`out`, one reading `.source`, `.version` or `.params`; in a custom tool's `run:`, one reading `.args`.

## Pipeline vars: `((var))` and `load_var:`

Separate a pipeline's **shape** from its **parameters**, so one file serves staging and production instead of being copy-pasted per environment and drifting:

```yaml test=templating-vars
resource_types:
- name: echoes
  config:
    check: |
      printf '[{"ref": "v1"}]'
    in: |
      echo {{ .source.uri | shellquote }} > uri.txt

resources:
- name: repo
  type: echoes
  source:
    uri: ((repo_uri))        # supplied at run time, not written in the file

jobs:
- name: build
  plan:
  - get: repo
  - task: show
    inputs: [repo]
    run: cat repo/uri.txt
    assert:
      # the exact value --var supplied (the command below) reached the template
      stdout: github.com/acme/app-staging
  assert:
    execution: [repo, show]
    outcome: succeeded
```

```bash
steps run pipeline.yml --job build --var repo_uri=https://github.com/acme/app-staging
steps run pipeline.yml --job build --vars-file prod.yml
```

Both compose: the file is the shared, checked-in set and a `--var` flag overrides it, which is the only ordering that makes a one-off override possible.

Capturing a value the run itself produces:

```yaml
jobs:
- name: release
  plan:
  - task: pick-tag
    outputs: [meta]
    run: printf 'v1.2.3\n' > meta/version.txt
  - load_var: tag
    inputs: [meta]           # the load_var step reads from its own declared input
    file: meta/version.txt
  - task: announce
    run: echo "releasing ((tag)) ok"
    assert:
      # the captured value, trimmed, substituted — untrimmed, version.txt's
      # trailing newline would land before "ok" and break this line
      stdout: releasing v1.2.3 ok
  assert:
    execution: [pick-tag, tag, announce]
    outcome: succeeded
```

The `inputs:` on the `load_var` step is not optional bookkeeping: a step's directory holds only the artifacts it declares, so a bare `file: version.txt` names nothing that exists. Both halves are checked at plan time — the file must sit inside a declared input, and that input must be something an earlier step produced.

The file is read on the machine running the pipeline, so it may not leave the step's directory: an absolute `file:`, or one whose `..` climbs out, is a load error. A `file:` built from a `((var))` is checked again once rendered, a symlink pointing outside the directory is refused, and a file above 1 MiB fails the step — a var is a value, not a data transfer.

`load_var:` names share the one `((var))` namespace, so a name both passed with `--var` and captured by a `load_var:` goes to `--var`: supplied vars substitute textually before the file is parsed, leaving no `((name))` for the captured value to fill — the `load_var:` step still runs, but substitutes nothing. (Concourse keeps run-captured vars in a separate `((.:name))` namespace instead; see [conformance.md](conformance.md).)

### ⚠️ Vars are config, not secrets

A substituted value is parsed, **hashed, and stored in `state.db`** like anything else written in the file. Vars separate shape from parameters; they are not a secret store. Keep credentials in the env-var references that exist for them (`api_key_env:`), which are read at run time and never enter the merkle content. Neither is masked: a step that prints a secret has it stored and shown as printed (see [conformance.md](conformance.md)).

### The rest

- **Substitution is textual and happens before the parse**, so a var can appear anywhere a value does — inside a URI, mid-command, as a whole mapping value — without steps maintaining a list of fields that might contain one.
- **An unresolved `((name))` is a load error**, naming the var. Left alone it would reach a shell as that literal text and fail somewhere far from the mistake. A name a later `load_var:` produces is fine; one nothing supplies is not. Every step is checked, hooks and blocks included, and a captured name counts only where the run guarantees the capture happened: after it in plan order (`do:` children included), but not in a sibling branch of the same `in_parallel:` or `race:` — whether one branch sees another's value would be a race. After an `in_parallel:` every branch's captures are available; after a `race:` only the ones every branch makes. A `try:` body's captures stay inside it, and a hook sees what preceded its step, plus the step's own captures in `on_success:` only.
- **A `load_var:` value is substituted before the step is hashed**, so two runs that captured different values never share a cache entry. Hashing the unsubstituted text would let a step that ran against `v1.2.3` satisfy a run that meant `v2.0.0`.
- **The captured value is trimmed.** The common way to produce one is `git describe > version.txt`, whose trailing newline would otherwise land in the middle of a command.
- **`load_var:` values are scoped to one job run.** A var captured in one run says nothing about the next.
