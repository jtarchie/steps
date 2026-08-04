# Proposal: Robustness Features (Concourse Gaps + Agent-Native Ideas)

A high-level survey of what Concourse has that `steps` doesn't, plus features
Concourse *can't* have because it doesn't run agents. Each entry is a sketch —
feature, example YAML, and how it would work — not an implementation plan.

Ordering note: `steps` already borrowed Concourse's step-**sequencing** surface
(`when:`, `to:`/`verdicts:`, hooks, `version:`, `resource:` aliasing,
`input_mapping:`). What's missing is almost entirely the **concurrency and
cross-job** dimension: nothing in a plan can run at the same time as anything
else, and nothing about job B's gets can depend on job A having succeeded.
Those two gaps (fan-out and fan-in) are Tier 1; everything else builds on them.

Every feature below follows the repo's two standing rules: absent, hashes and
behavior are byte-identical to today; present, new fields fold into a node's
hashed content only when set (value-gated).

---

## Tier 1 — Concourse core gaps

### 1. `in_parallel` — fan-out within a plan

The single biggest robustness gap. Today a plan is strictly sequential: three
independent gets, or a lint task and a test task, serialize for no reason — and
one slow resource check stalls everything behind it.

```yaml
jobs:
  - name: verify
    plan:
      - in_parallel:
          limit: 2          # max in flight (default: unbounded)
          fail_fast: true   # first failure cancels the rest
          steps:
            - get: repo
            - get: rules
            - task: lint
              run: golangci-lint run
            - task: test
              run: go test ./...
```

**How it works.** A new step kind holding a `steps:` list. `RunJob` launches
each branch in a goroutine bounded by `limit`, waits on all, and the step's
outcome is the aggregate (any failure = failure; `fail_fast` cancels the
sibling contexts). Each branch gets its own merkle node exactly as if it were
sequential, so caching is unchanged — a cached branch just returns instantly.
Branch steps can't use `to:` targets outside their own branch (same rule as
hook steps). The store is already safe for this (`SetMaxOpenConns(1)`
serializes writes); workspace `strategy: copy`/`btrfs` already gives each step
an isolated view, which becomes the recommended pairing for parallel tasks
that write.

### 2. `passed:` — cross-job fan-in on get steps

The heart of Concourse's pipeline model, and the true "fan-in": *only run this
job against versions that already survived these other jobs.* Today `steps
watch` triggers every affected job independently — nothing prevents `deploy`
from running a commit that `test` failed.

```yaml
jobs:
  - name: unit
    plan:
      - get: repo
        trigger: true
      - task: test
        run: go test ./...

  - name: deploy
    plan:
      - get: repo
        trigger: true
        passed: [unit, lint]   # only versions green in BOTH jobs
      - put: release
```

**How it works.** The store already records which version each job ran and its
outcome (`resource_checks`, job runs). `passed:` adds a query at
resolve/trigger time: instead of "latest version from check," it's "latest
version that has a successful run of every listed job." The poller enqueues
`deploy` only when such a version exists, which also gives diamond fan-in for
free (two jobs feed one) — the shared-version join is what makes it a fan-in
rather than two races. Hash-wise the pinned version is already in the get
node's content, so nothing new to fold in beyond the `passed:` list itself.

### 3. `attempts:` and `timeout:` on every step

Agent steps already have `attempts:`; tasks, puts, and gets don't — yet flaky
`gh` calls and hung resource checks are the most common real-world failures.

```yaml
- get: repo
  attempts: 3
  timeout: 2m
- task: flaky-integration
  run: ./ci/integration.sh
  attempts: 2
  timeout: 30m
```

**How it works.** `timeout:` is a `context.WithTimeout` wrapper around the
step's execution (the plumbing already exists — everything takes a ctx, and
`internal/outcome.Classify` already distinguishes errored from failed, so a
timeout classifies as `errored` and fires `on_error`). `attempts:` generalizes
the agent-step field to the flat `Step` union, reusing `internal/retry`.

### 4. `try:` — tolerated failure

Hooks handle *reacting* to failure; nothing handles *shrugging at* it. A
best-effort notification or metrics push shouldn't fail the build.

```yaml
- try:
    put: slack-notify
    params:
      text: "build finished"
```

**How it works.** A wrapper step kind: run the inner step, record its real
outcome for observability/asserts, but report success to the plan walker.
Equivalent to `on_error`+`on_failure` swallowing, but declarative and
composable with `timeout:`/`attempts:` (retry, then give up quietly).

### 5. `across:` — matrix fan-out

Run the same step(s) once per value combination — Concourse's newest step
modifier, and a natural fit for "run this agent against every package."

```yaml
- across:
    - var: go_version
      values: ["1.25", "1.26"]
    - var: package
      values: [internal/agent, internal/pipeline]
  task: matrix-test
  image: "golang:{{ .vars.go_version }}"
  run: go test ./{{ .vars.package }}/...
```

**How it works.** Plan expansion, not runtime magic: at plan time each value
combination becomes its own node with the vars folded into its hashed content
(so one changed cell re-runs one cell, not the matrix — this is where merkle
caching genuinely outshines Concourse, which re-runs everything). Execution
reuses `in_parallel`'s machinery with `max_in_flight`. The template layer
gains a `.vars` namespace alongside `.source`/`.params`. The `get ...
version: every` fan-out is the same shape and could share the implementation.

### 6. `serial:` / `serial_groups:` — job-level mutual exclusion

`steps watch --max-concurrent 2` can currently run the same job twice
concurrently against two versions, or run two jobs that both `put` the same
resource. Concourse's answer:

```yaml
jobs:
  - name: deploy-staging
    serial: true                 # never two builds of me at once
  - name: deploy-prod
    serial_groups: [deploy-lock] # never concurrent with anyone in the group
```

**How it works.** The watch worker pool checks a lock table (or in-process
lock keyed by group name — the queue is already in SQLite, so a `locks` table
survives crashes) before dequeuing; a held lock re-queues the run. Purely a
`internal/trigger` concern; `RunJob` never knows.

### 7. Pipeline vars — `((var))` interpolation and `load_var`

Today secrets ride in via `api_key_env` and everything else is hard-coded in
the YAML. Concourse separates pipeline *shape* from pipeline *parameters*.

```yaml
# pipeline.yml
resources:
  - name: repo
    type: git-like
    source:
      uri: ((repo_uri))

# steps run pipeline.yml --job build --var repo_uri=https://... \
#   or --vars-file prod.yml
```

Plus the runtime form — capture a value mid-build and use it later:

```yaml
- task: pick-tag
  run: git describe --tags > version.txt
- load_var: tag
  file: version.txt
- put: release
  params:
    tag: "{{ .vars.tag }}"
```

**How it works.** `((var))` is a load-time textual substitution in
`LoadConfig` before YAML parsing (Concourse semantics), so hashes naturally
reflect the *resolved* values — same pipeline + different vars = different
cache keys, which is correct. `load_var` is a tiny step kind that reads a
workspace file into the `.vars` template namespace for subsequent steps; the
value folds into downstream nodes' content like a pinned version does.

### 8. Webhook-triggered checks

Polling (`check_interval`) burns rate limits and adds latency. Concourse
resources accept `webhook_token`; `steps watch` could listen.

```yaml
resources:
  - name: repo
    type: git-like
    source: {uri: ...}
    webhook_token: ((hook_token))
# steps watch pipeline.yml --listen :8080
# POST /check/repo?token=... => immediate check of that resource
```

**How it works.** An opt-in HTTP listener inside `steps watch` whose only
action is "run `pollOnce` for this resource now" — the webhook body is
untrusted and ignored; it's a doorbell, not a data source (keeps the trust
boundary trivial). Polling continues as the fallback.

---

## Tier 2 — agent-native robustness (no Concourse equivalent)

### 9. Ensemble agents — parallel fan-out, verdict fan-in

The agent version of redundancy: N cheap reviewers in parallel, one
aggregation rule. Catches the single-model blind spot the same way RAID
catches a disk.

```yaml
- ensemble:
    agents:
      - {agent: reviewer-a, prompt: "Review the diff for correctness."}
      - {agent: reviewer-b, prompt: "Review the diff for correctness."}
      - {agent: reviewer-c, prompt: "Review the diff for correctness."}
    verdicts: [approve, reject]
    decide: majority        # or: unanimous, any, or an agent name to judge
  to:
    approve: publish
    reject: revise
```

**How it works.** Composition of existing pieces: each member runs the normal
verdict-mode conversation loop (`in_parallel` underneath), then `decide:`
reduces N verdicts to one — `majority`/`unanimous`/`any` are pure functions;
naming an agent instead runs a judge step whose prompt is assembled from the
members' recorded runs (the `handoff:` context builder already knows how to
render a prior run). The reduced verdict routes through `to:` exactly like a
single agent's would. Each member is its own merkle node; the ensemble node
hashes the member list + decide rule.

### 10. `race:` — speculative execution, first success wins

For expensive-but-flaky work: try the cheap path and the reliable path at
once, keep whichever lands first.

```yaml
- race:
    steps:
      - {agent: fast-model, prompt: "Summarize the release."}
      - {agent: strong-model, prompt: "Summarize the release."}
```

**How it works.** `in_parallel` with inverted aggregation: first branch to
*succeed* cancels the siblings (ctx cancel — the conversation loop already
honors ctx); the race fails only if all branches fail. The winning branch's
outputs become the step's outputs, so branches should declare identical
`outputs:`. Needs workspace isolation so losers can't half-write shared state
— a load-time "race requires workspace:" rule keeps it honest.

### 11. Budgets — token/cost/wall-clock caps with graceful degradation

The agent analogue of `timeout:`. A revise loop (`to:` + `max_visits:`)
bounds *iterations*; nothing bounds *spend*.

```yaml
agents:
  - name: writer
    source: {...}
    budget:
      tokens: 200000        # per invocation
jobs:
  - name: publish
    budget:
      cost: "$2.50"         # whole-job ceiling, all agent steps combined
    plan: [...]
```

**How it works.** The conversation loop already sees usage numbers on every
model response; a budget is a running counter checked between turns. Blowing a
step budget classifies as `errored` (fires `on_error`, distinct from the model
*failing*); a job budget aborts remaining agent steps. Not hashed — a budget
is an operational limit, not content, same treatment as `assert:`. A natural
extension is `on_budget: {agent: cheaper-writer}` — degrade instead of die.

### 12. Approval steps — a human in the plan

Agent pipelines eventually gate something irreversible (a `put` that
publishes). The most robust guard is the oldest one.

```yaml
- approval:
    message: "Draft is in draft/summary.md — publish?"
    timeout: 24h
- put: blog
```

**How it works.** In `steps run`, an interactive y/N prompt. Under
`steps watch`, the job parks: the trigger queue already persists pending work,
so the approval becomes a queue state (`awaiting-approval`) surfaced by a
`steps approvals` / `steps approve <job> <id>` subcommand — and the webhook
listener (#8) gives remote approval for free. An expired timeout classifies as
`aborted` (the outcome vocabulary already exists). Never hashed, never cached.

### 13. Agent source failover

Provider outages are the flakiest dependency an agent pipeline has, and
`attempts:` against a dead endpoint just fails slower.

```yaml
agents:
  - name: writer
    source:
      model: big-model
      api_key_env: PRIMARY_KEY
    fallback:
      - source:
          endpoint: https://backup-provider/v1
          model: equivalent-model
          api_key_env: BACKUP_KEY
```

**How it works.** `provider.go` builds a client list instead of a client;
connection-level errors (not model refusals or tool errors) advance to the
next source, and `attempts:` applies per source. Which source actually served
a cached run doesn't change what was produced, so only the primary source
folds into `AgentContentMap` — fallback is availability, not content. Same
endpoint validation (`validateAgentEndpoints`, no userinfo) applies to every
fallback entry.

### 14. Watch circuit breaker

A broken job under `steps watch` retriggers on every new version forever —
burning model spend on a failure no retry will fix.

```yaml
jobs:
  - name: nightly-summary
    max_consecutive_failures: 3   # then pause; steps jobs resume <name>
```

**How it works.** Entirely inside `internal/trigger`: the store records run
outcomes already, so the poller skips enqueuing a job whose last N runs all
failed, logs loudly, and a `resume` subcommand (or any manual `steps run`
succeeding) resets the counter. The agent-native touch: tripping the breaker
can fire the job's `on_error` hook chain, which may itself be an agent that
files the issue.

---

## Smaller ideas (one-liners worth keeping on the radar)

- **`get` params** — Concourse passes `params:` to `in` (shallow clone, submodules); `steps` gets currently take none.
- **Build retention / `steps history <job>`** — the store keeps everything; there's no CLI to browse or prune it.
- **`steps validate`** — run all of `LoadConfig` + `ValidateArtifactFlow` + transition validation without executing anything (CI for the pipeline file itself).
- **Structured artifact asserts** — `assert: {files: [draft/summary.md]}` alongside the existing output/exit-code asserts, so "the agent said it wrote the file" is verified against the workspace.
- **Per-resource `check_interval`** — one global poll cadence penalizes cheap checks or hammers expensive ones.
- **Pinned-version override at the CLI** — `steps run --version repo=abc123` for reproducing an old build without editing YAML (the plumbing exists via `version:` pinning).

## Suggested sequencing

`in_parallel` (#1) is the keystone — `across:`, `ensemble:`, and `race:` are
all aggregation policies over the same branch runner. `passed:` (#2) is
independent and the highest-value single feature for multi-job robustness.
`attempts:`/`timeout:`/`try:` (#3–4) are small, orthogonal, and could ship
first. Everything in Tier 2 composes from Tier 1 machinery plus code that
already exists (verdict loop, handoff rendering, trigger queue, outcome
classification).

## Where these live now

Each feature above was broken out into a story and is now tracked as a GitHub
issue, labeled by tier — see [the issue list](https://github.com/jtarchie/steps/issues).
The issues are the source of truth for scope, acceptance criteria, and status;
this document stays as the survey they were derived from.

Cross-cutting notes surfaced while breaking them out, which no single issue
owns:

- **`attempts:`/`timeout:`, `try:`, and budgets share an outcome-classification
  requirement**: timeouts and budget breaches should classify as `errored`, not
  `failed`, so `on_error` hooks (not `on_failure`) fire — consistent with
  existing `outcome.Classify` semantics.
- **`in_parallel` is the keystone**: `across:`, ensemble agents, and `race:`
  all reuse its branch-runner and cancellation semantics rather than inventing
  their own.
- **Pipeline vars and budgets both touch secret handling**: a `((var))` sourced
  from a vars-file and a cost-budget's spend total both risk landing sensitive
  or operational data in `state.db` if not deliberately excluded from hashing —
  worth reviewing together against the existing `api_key_env`/`endpoint:`
  precedent this repo's trust-boundary rules already set elsewhere.
- **Webhooks, approval steps, and the watch circuit breaker all need an
  outbound notification path** (webhook received, approval pending, breaker
  tripped) — likely worth designing once as a shared "notify" mechanism (e.g. a
  hook-triggered `put`) rather than three bespoke ones.
