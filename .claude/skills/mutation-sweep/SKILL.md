---
name: mutation-sweep
description: Periodic mutation-testing sweep of the steps repo — picks the package that has churned most since it was last swept, measures it, triages every surviving mutant (killed by e2e / equivalent / real gap), writes the missing tests, and updates tools/mutation-ledger.json. Use when the user asks for a mutation sweep, a gremlins or mutants run, "which tests are vacuous", or to check how stale mutation coverage is. Not for the doc-corpus mutation suites in ./e2e (those run in `task test`; see steps-tests).
---

# steps: the mutation sweep

Mutation testing cannot gate a commit — a heavy package costs hours and a survivor needs judgment — so it runs as a sweep, by hand, and **`tools/mutation-ledger.json` is what makes "periodic" mean something without a scheduler**: one row per package with the commit it was last swept at, so staleness is a `git diff --stat`, not a memory.

## Measuring is not sweeping

**Measuring** a package is machine time: run `tools/mutants sweep`, write the ledger row. **Triaging** its survivors is judgment. They cost differently by two orders of magnitude (merkle: two minutes to measure, ninety to triage), so they are separate commands. `task mutate-all` measures EVERY package unattended and triages none — it is how the ledger gets a real floor and a survivor count for each package, and it belongs to a night the machine sits idle. This skill is the other half: one package's survivors, worked through.

`tools/mutants record` refuses to write an efficacy LOWER than the row already holds. That refusal is the finding: triage what now survives, and change the number by hand only once the drop is understood.

## 1. Choose what to sweep

- `task mutate-stale` prints every ledger package with the lines changed under it since its `swept_at` commit, most first; a package missing from the ledger has never been measured and sorts first. **Once `task mutate-all` has filled the ledger, choose by survivors and stakes instead**: most `lived`, weighted by what a wrong answer costs — `internal/store/sqlite` (retention DELETES rows; scoping keeps one pipeline out of another's cache) outranks a larger count in a package whose survivors are mostly cosmetic. One package per sweep; a second only if the first took under ~20 minutes.
- **When a sweep is due**: `task` says so in one line at the end of a green run (a package never measured, or one that has drifted 300+ lines since `swept_at`). Before any `v*` tag, every row should be measured and at or above its floor.
- The user naming a package overrides the ranking.
- Run `task vuln` first. There is no CI and no cron here, so a sweep is the only thing that notices a CVE published between commits.

## 2. Run it where nothing else is

- **How it works, because triage leans on it**: `tools/mutants sweep` (behind `task mutate-pkg`) profiles every test of the package in a process of its own, first all of them in parallel with `DOCKER_HOST` pointed nowhere — a test that SKIPS there is a docker test, which is how tests are classified without a list — then the docker ones serially for real. Each mutant (gremlins v0.6.0's five default operators, gremlins' coverage rule) is overlaid with `go test -c -overlay` and run against ONLY the tests whose profile executes its line: the non-docker ones in parallel, then the docker ones one at a time for survivors. The report is `.gremlins/<pkg>.json` in gremlins' shape plus `killed_by`. It replaced gremlins, which ran the whole suite per mutant: venue 11h, pipeline 8.7h, agent 7.5h.
- **Not while `task` is running anywhere on the machine** (`pgrep -fl 'go test|golangci-lint|custom-gcl'`). Load makes mutants time out, and a timeout is not a verdict.
- **A test failing unmutated stops the sweep**, in either profiling pass: a mutant it "killed" would be a lie. The parallel pass is also the check that this package's tests survive running beside each other; one that does not needs fixing (or `WORKERS=1`), not ignoring.
- **The main checkout is fine**: nothing is copied, a mutant is an overlay. Do not edit the package's files mid-run — the profiles and the overlays both read them.
- `SINCE=<swept_at>` restricts the mutants to files changed since that commit; a scoped run is not a measurement of the package, so record its row by hand if at all. A subdirectory is its own package and never swept with its parent.
- Timeouts are 15× what the covering tests took unmutated, plus 10s: a mutant that loops dies in seconds rather than under a whole-suite budget (a retry-loop mutant once reached 174GB under gremlins').
- **Stopping it**: Ctrl-C/SIGTERM reaches `go run`, not necessarily the test binaries. `pkill -f 'mutants-sweep-'` takes the rest (their binaries live under `$TMPDIR/mutants-sweep-*`, which you then remove). **A SIGKILLed test binary runs no cleanup**: if it was a docker test, `docker ps` and remove the `steps-<hash>` containers minutes old — they are yours.
- **What it cannot see**: a statement executed only when tests run TOGETHER (state one leaves for the next) is covered by no single test and scores NOT COVERED where gremlins would have run it. Rare; a NOT COVERED line a test plainly reaches is that.

## 3. Triage every LIVED mutant — three outcomes, no fourth

Read `.gremlins/<pkg>.json`; for each `LIVED` (and spot-check `TIMED OUT` — confirm a hang by stack, not by assumption):

1. **e2e kills it.** A package's sweep never runs `./e2e`, and most survivors on old lines die there. Apply the mutant by hand, run `go test ./e2e -run <the nearest test> -count=1 -timeout 300s`. Fails → covered; record nothing.
2. **Equivalent.** The mutant changes no observable behaviour (`<` vs `<=` on a value that is never equal; a changed constant only a log line reads). Add it to the ledger row's `equivalent` list with the reason, keyed `file | mutator | trimmed source line` — line numbers rot, source text mostly does not. The next sweep skips anything still matching.
3. **A real gap.** Write the test. The applied mutant IS the sabotage CLAUDE.md requires: watch the new test fail against it, restore, watch it pass. Follow the repo's outside-in order — if the behaviour is user-visible, the test is an e2e, not a unit test next to the line.

Sabotage hygiene, each of which has cost an hour before: back the file up to the scratchpad under a unique name and restore with a `trap`; give every sabotaged `go test` a `-timeout`; `git status` afterwards — sabotaging path code drops stray directories under `e2e/`, and the commit hook will refuse them anyway.

## 4. Record and commit

- Update the package's ledger row: `swept_at` (the commit swept), `date`, `killed`, `lived`, `timed_out`, `not_covered`, `efficacy`. **Efficacy is a floor that only rises**: if it fell, say so in the report and do not lower it silently — the drop is the finding.
- Report to the user: package, efficacy before → after, tests added, equivalents recorded, anything left untriaged and why.
- New tests land like any other change: `task` green, then commit to main. The ledger update rides in the same commit as the tests it records.

## Ledger shape

```json
{
  "packages": {
    "./internal/merkle": {
      "swept_at": "2a2c096",
      "date": "2026-09-18",
      "killed": 41, "lived": 0, "timed_out": 0, "not_covered": 2,
      "efficacy": 100.0,
      "note": "",
      "equivalent": [
        {"mutant": "merkle.go | CONDITIONALS_BOUNDARY | if len(parts) < 2 {", "why": "len is never exactly 2 here: callers pass 1 or 3"}
      ]
    }
  }
}
```
