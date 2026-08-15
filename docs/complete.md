# A complete pipeline

Everything the other pages introduce piece by piece, in one working pipeline: a resource fetched, a task preparing context, an agent that reads, writes an artifact, and decides — its verdict routing the plan — and a put publishing what it produced. Like every example in these docs, this runs under the test suite exactly as shown:

```yaml test=complete-review
resource_types:
- name: notes                    # a self-contained stand-in for git/an API
  config:
    check: |
      printf '[{"ref": "v1"}]'
    in: |
      echo 'The widget catalog is seeded from widgets.json on boot.' > NOTES.txt
    out: |
      echo publishing:
      cat report/summary.md

resources:
- name: repo
  type: notes
  source: {}
- name: results
  type: notes
  source: {}

agents:
- name: reviewer
  source: { model: openrouter/qwen/qwen3.7-flash }
  system: You review release notes. Be terse.
  tools: [read_file, write_file]

jobs:
- name: review
  plan:
  - get: repo                          # fetch: NOTES.txt lands in repo/
    trigger: true                      # steps watch re-runs this on a new version
  - task: prepare
    outputs: [guidelines]
    run: echo 'summaries must be one line' > guidelines/RULES.txt
  - agent: reviewer
    inputs: [repo, guidelines]         # sees exactly these two artifacts
    outputs: [report]                  # and captures this one back
    context_paths: [guidelines/RULES.txt]   # handed to the model at turn zero
    max_turns: 8                            # read, write, decide, with room to spare
    prompt: "Read repo/NOTES.txt and write a one-line summary to report/summary.md."
    verdicts:
      - approve: results               # the verdict picks the next step
      - reject: escalate
    assert:
      verdict: approve                 # what it decided
      files: [report/summary.md]       # ...and that it actually wrote the thing
  - task: escalate
    run: echo paging a human
  - put: results                       # out: reads the agent's artifact
    inputs: [report]
  assert:
    execution: [repo, prepare, reviewer, results]   # escalate is absent — the
    outcome: succeeded                              # approve verdict routed past it
```

What each piece is doing, with the page that explains it:

- **`resource_types:`/`resources:`** — the `check`/`in`/`out` contract; here self-contained shell so the pipeline runs anywhere. [resources.md](resources.md)
- **`inputs:`/`outputs:`** — every step names what it reads and what it keeps; the agent cannot see anything it didn't declare. [workspace.md](workspace.md)
- **`context_paths:`** — the guidelines arrive as a synthetic `read_file` result, no turn spent fetching them. [agents.md](agents.md#context_paths--files-delivered-as-synthetic-read_file-results)
- **`verdicts:`** — the synthesized required verdict tool; `approve` jumps to the put, `reject` to the escalation. [control-flow.md](control-flow.md#step-transitions-tomax_visitsverdicts)
- **`trigger:`** — under `steps watch`, a new version runs the job; under `steps run`/`steps test` it's inert. [infra.md](infra.md#downstream-triggers-trigger-true--steps-watch)

Run it:

```bash
steps run pipeline.yml          # one shot
steps watch pipeline.yml        # keep running it on new versions
steps web pipeline.yml          # watch the transcript in a browser
```

From here, the usual next steps are wiring in a real resource (the built-in `git`, or your own type against an API), swapping the model for the one you use ([agents.md](agents.md)), and bounding the spend with `budget:` and `timeout:` ([attempts-timeout.md](attempts-timeout.md)).

For the full-scale version — an adaptive PR review whose matrix width a planner step decides mid-run, concurrent reviewer cells, collected findings, a synthesizer, and a human approval gate — see [`examples/pr-review.yml`](../examples/pr-review.yml) in the repo, the one example that runs against a real model and a real repository rather than inside the test suite.
