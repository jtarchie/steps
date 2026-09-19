---
name: mutation-sweep
description: Periodic gremlins mutation-testing sweep of the steps repo — picks the package that has churned most since it was last swept, runs it in an isolated worktree, triages every surviving mutant (killed by e2e / equivalent / real gap), writes the missing tests, and updates tools/mutation-ledger.json. Use when the user asks for a mutation sweep, a gremlins run, "which tests are vacuous", or to check how stale mutation coverage is. Not for the doc-corpus mutation suites in ./e2e (those run in `task test`; see steps-tests).
---

# steps: the mutation sweep

Gremlins cannot gate a commit — a heavy package costs hours and a survivor needs judgment — so it runs as a sweep, by hand, and **`tools/mutation-ledger.json` is what makes "periodic" mean something without a scheduler**: one row per package with the commit it was last swept at, so staleness is a `git diff --stat`, not a memory.

## Measuring is not sweeping

**Measuring** a package is machine time: run gremlins, write the ledger row. **Triaging** its survivors is judgment. They cost differently by two orders of magnitude (merkle: two minutes to measure, ninety to triage), so they are separate commands. `task mutate-all` measures EVERY package unattended and triages none — it is how the ledger gets a real floor and a survivor count for each package, and it belongs to a night the machine sits idle. This skill is the other half: one package's survivors, worked through.

`tools/mutants record` refuses to write an efficacy LOWER than the row already holds. That refusal is the finding: triage what now survives, and change the number by hand only once the drop is understood.

## 1. Choose what to sweep

- `task mutate-stale` prints every ledger package with the lines changed under it since its `swept_at` commit, most first; a package missing from the ledger has never been measured and sorts first. **Once `task mutate-all` has filled the ledger, choose by survivors and stakes instead**: most `lived`, weighted by what a wrong answer costs — `internal/store/sqlite` (retention DELETES rows; scoping keeps one pipeline out of another's cache) outranks a larger count in a package whose survivors are mostly cosmetic. One package per sweep; a second only if the first took under ~20 minutes.
- **When a sweep is due**: `task` says so in one line at the end of a green run (a package never measured, or one that has drifted 300+ lines since `swept_at`). Before any `v*` tag, every row should be measured and at or above its floor.
- The user naming a package overrides the ranking.
- Run `task vuln` first. There is no CI and no cron here, so a sweep is the only thing that notices a CVE published between commits.

## 2. Run it where nothing else is

- **In a git worktree** (`EnterWorktree`, or an `isolation: worktree` agent), never the main checkout. Gremlins mutates copies, but its coverage phase runs in the tree it was started from, and another session editing that tree mid-run breaks the run. A worktree also holds only tracked files, so gremlins stops copying `.claude/worktrees/` into every mutant directory.
- **Not while `task` is running anywhere on the machine** (`pgrep -fl 'go test|golangci-lint'`). Load makes mutants time out, and gremlins scores TIMED OUT as KILLED — a loaded run reports tests that do not exist.
- `task mutate-pkg PKG=./internal/foo` does the mechanics: cleans the test cache first (a cached coverage run gives a ~1s baseline, and then every mutant "times out"), coefficient 15, results to `.gremlins/<pkg>.json`. Measured: `./internal/merkle`, 166 mutants, 2m14s, zero timeouts.
  - **Package mode is the default, and `INTEGRATION=1` is not a sweep.** It runs `go test ./...` — docker e2e included — once per mutant, several at a time: merkle did not finish in ten minutes that way. Use it only to get a final answer on a handful of named survivors, and prefer step 3's targeted `go test ./e2e -run` even then.
  - `WORKERS=1` for a package whose tests bind ports, spawn containers or share the docker daemon (`venue`, `shell`, `web`, `cli`, `pipeline`): parallel mutants otherwise fail each other's tests and score KILLED for the wrong reason.
  - **Heavy packages get scoped, because `--diff` is broken in package mode** (it compares repo-relative diff paths to package-relative mutant paths and skips everything). `task mutate-pkg PKG=./internal/venue SINCE=<swept_at>` generates `-E` excludes for every file unchanged since that commit. First sweep of a heavy package: do it file-group by file-group across several sweeps, recording which in the ledger row's `note`.
  - Package mode walks subdirectories: `./internal/store` ALONE reports exactly `sqlite`'s 343 mutants as its own. `task mutate-all` excludes children for you; by hand, pass `-E '^sqlite/' -E '^storetest/'`.
- **Stopping it**: SIGTERM is ignored while its `go test` child runs. `pkill -9 -x gremlins`, then kill the children; check with `pgrep -x gremlins`, never `pgrep -f` (it matches your own shell). Remove `$TMPDIR/gremlins-*` afterwards. **A SIGKILLed test binary runs no cleanup**: if the killed tests were docker-backed, `docker ps` afterwards and remove the `steps-<hash>` containers that are minutes old — they are yours.

## 3. Triage every LIVED mutant — three outcomes, no fourth

Read `.gremlins/<pkg>.json`; for each `LIVED` (and spot-check `TIMED OUT` — confirm a hang by stack, not by assumption):

1. **e2e kills it.** Package mode never runs `./e2e`, and most survivors on old lines die there. Apply the mutant by hand, run `go test ./e2e -run <the nearest test> -count=1 -timeout 300s`. Fails → covered; record nothing.
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
