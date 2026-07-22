---
target: go run main.go run ... (steps CLI output/UX)
total_score: 25
max_score: 40
na_heuristics: 
p0_count: 0
p1_count: 2
timestamp: 2026-07-22T16-12-45Z
slug: main-go-steps-run-cli
---
Method: dual-agent (A: a34b466f2e7da8c9a · B: a47230a4ee7b3c2c6), adapted for a terminal-only target — Assessment B substituted actual CLI execution + source grep for the usual markup detector/browser scan, since there is no HTML/CSS surface to run those against.

## Design Health Score

Mode: Operate (the visitor completes a task — running/testing pipelines). All 10 heuristics apply; none are n/a on this surface.

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | A cached step chain prints only the *first* `skip:` line, then nothing for the rest of the chain — later steps vanish from the transcript entirely. |
| 2 | Match System / Real World | 3 | `get`/`put`/`task` vocabulary matches the stated Concourse-style domain. |
| 3 | User Control and Freedom | 3 | Ctrl-C is clean: `signal.received signal=interrupt` → wrapped cancellation error → exit 1, no hang. |
| 4 | Consistency and Standards | 2 | Every error prints twice — once as a structured `ERR` log line, once as a plain `steps: error: ...` line — identical content, no differentiation. |
| 5 | Error Prevention | 2 | Bad job names get a helpful "available: [...]" list before you retype anything, but YAML errors are bare (`yaml: line 39: found a tab character...`) with no snippet/column. |
| 6 | Recognition Rather Than Recall | 3 | `--help` lists each flag's env var inline (`--log-level="info" ... ($STEPS_LOG_LEVEL)`); errors list valid job names. |
| 7 | Flexibility and Efficiency | 2 | No `--json`/`--quiet` machine-readable mode and no `--version` flag, despite this being a CI-adjacent tool people will script around and file bugs against. |
| 8 | Aesthetic and Minimalist Design | 2 | Double-printed errors are pure noise; step output, framework logs, and pass/fail lines all share one visual weight with no indentation. |
| 9 | Error Recovery | 3 | Errors are specific and wrapped with step context (`step 0 (task "wait"): command "sleep 5" failed: context canceled`). |
| 10 | Help and Documentation | 3 | `--help` text is tight, accurate, and complete for the 5 subcommands that exist. |
| **Total** | | **25/40** | **Acceptable** — solid bones, several fixable regressions keep it out of "Good." |

## Design Specificity Verdict

**LLM assessment**: This clearly isn't `log.Println` bolted onto a Go CLI as an afterthought — `tint` gives leveled, colored, source-located structured logs, kept visually distinct from a separate plain-text "progress" channel (`task: scout`, `get: prs (version: ...)`, `skip: build`), and error messages consistently name what failed and offer the valid alternative rather than a bare Go error string. That's considered. But the polish stops short of end-to-end: color is unconditional (no TTY check anywhere in the codebase — confirmed by grep, zero hits for `isatty`/`IsTerminal`/`NO_COLOR`), errors double-print, and there's no version string to discoverable-report. The result reads as a tool with real design intent in its happy path that hasn't had the same attention applied to its edges (piped output, CI logs, error hygiene, bug reports).

**Deterministic scan** (Assessment B, hand-verified in place of a markup detector): `grep -rn "isatty\|IsTerminal\|NO_COLOR" internal/ main.go` → zero matches. `main.go:365` calls `tint.NewTextHandler(os.Stderr, ...)` unconditionally, no TTY branch. `cat -v` on piped/redirected/`NO_COLOR=1` runs shows byte-identical ANSI escapes (`^[[2m`, `^[[91m`, `^[[92m`, `^[[0m`) in every condition. No `--version` flag exists at all — `steps --version` is swallowed by `run`'s unrelated `--version=KEY=VALUE` version-*pin* flag and errors with a confusing `missing value, expecting "<key>=<value>;..."` message. No browser/detector findings apply (no markup surface); this section is the CLI's deterministic equivalent.

## Overall Impression

The happy path is genuinely good — clear verb-prefixed progress lines, helpful "did you mean" style errors, clean Ctrl-C. The single biggest opportunity is closing the gap between that happy-path polish and what actually happens at the edges: piped/redirected output, a cache hit mid-chain, and any error path all currently leak or omit information a user needs. None of these are hard fixes; they're all things the codebase already has the pieces for (tint's own `NoColor` option, the existing per-step logging, kong's error path) that just haven't been wired up consistently.

## What's Working

- **Actionable errors, not bare Go strings.** A bad `--job` value returns `no job named "does-not-exist" (available: [gate-open gate-closed nonzero-is-false converge exhaust passing failing])` — it hands you the fix in the same line, verified verbatim by Assessment B.
- **Clean interrupt handling.** SIGINT produces `signal.received signal=interrupt`, a properly wrapped cancellation error, and a non-zero exit — no orphaned process, no goroutine hang, confirmed by direct testing.
- **Debug output is opt-in and genuinely useful when asked for.** `STEPS_LOG_LEVEL=debug` adds real diagnostic value (per-step `shell.run`/`shell.capture_full` with command + cwd + exit code, workspace create/remove paths) without polluting the default `info` output — matches the project's own stated intent in CLAUDE.md.

## Priority Issues

**[P1] ANSI color is unconditional — contradicts the project's own accessibility commitment.**
Why it matters: PRODUCT.md commits to "graceful behavior without ANSI color when piped or unsupported" for exactly this reason (screen readers, non-visual terminal clients, log files). Right now every `ERR`/`INF`/`DBG` line carries raw escape codes (`^[[2m`, `^[[91m`, `^[[92m`) whether the output is a live TTY, piped through `cat`, redirected to a file, or run with `NO_COLOR=1` set — verified byte-for-byte identical across all four conditions. A screen reader or a `steps run ... > build.log` capture gets literal escape-code noise inline with every log line, which is worse than plain text, not neutral.
Fix: gate `tint.Options{NoColor: ...}` in `main.go`'s `initLogging` on `golang.org/x/term.IsTerminal(int(os.Stderr.Fd()))` and/or a `NO_COLOR` env check — two lines, no new dependency (`x/term` is already an indirect dependency via the btrfs backend's `x/sys`).
Suggested command: `/impeccable harden` (this is exactly a production-readiness/edge-case gap, not a taste call).

**[P1] A cached step chain goes silent after its first skipped step.**
Why it matters: re-running a job whose steps are unchanged prints `skip: <first-step>` and then jumps straight to `job.done` — later steps in the same chain (e.g. a job with `build` then `test`) are never mentioned at all, not even as `skip:`. A user scanning the transcript can't tell whether `test` ran, was cached, or silently got dropped — confirmed by running the same job twice and diffing output. This directly undercuts heuristic 1 (visibility of status), which is otherwise this CLI's strongest area.
Fix: in `internal/pipeline` (wherever the merkle-skip short-circuits the remaining chain), keep emitting one `skip: <name>` line per remaining step instead of returning after the first.
Suggested command: `/impeccable clarify` (it's a missing status line, not a structural redesign).

**[P2] Every error prints twice with no differentiation.**
Why it matters: a bad job name (or any other failure) logs the identical message on two consecutive lines — once via `slog.Error("main.run", "error", err)`, once via `fmt.Fprintf(os.Stderr, "steps: error: %v\n", err)` — pure repetition, confirmed verbatim by Assessment B on every error case tested (bad job, missing file, malformed YAML). It's aesthetic noise (heuristic 8) that also slightly undercuts trust in the tool's polish once you notice it.
Fix: pick one channel — structured log only at `debug`/above, plain `steps: error:` line otherwise — rather than both unconditionally.
Suggested command: `/impeccable distill`.

**[P2] No `--version` flag, and the reserved name is confusing.**
Why it matters: there's no way to print what build of `steps` you're running, which matters the moment someone files a bug or diffs behavior across installs. Worse, `steps --version` doesn't error with "unknown flag" — it silently resolves to `run`'s unrelated `--version=KEY=VALUE` version-*pin* flag and fails with `--version: missing value, expecting "<key>=<value>;..."`, an actively misleading message for someone who typed the single most conventional CLI flag there is.
Fix: add a real top-level `--version`/`-v` via kong's `kong.VersionFlag` wired to a build-time version string; consider whether the pin flag's name should change to avoid the collision (or at least document it in `--help`).
Suggested command: `/impeccable clarify`.

**[P3] `steps test`'s all-pass run has no closing tally.**
Why it matters: confirmed directly in `main.go`'s `TestCmd.Run` — each job prints its own `PASS <name>`/`FAIL <name>: <err>` line, but when every job passes, the loop just ends; there is no final `N/N passed` line. (A failing run does get an implicit tally via the returned `test: %d job(s) failed: [...]` error, but the success path has nothing symmetrical.) A user has to count lines themselves to confirm total coverage.
Fix: print a final `fmt.Printf("%d/%d passed\n", len(cfg.Jobs)-len(failures), len(cfg.Jobs))` before returning.
Suggested command: `/impeccable clarify`.

## Persona Red Flags

**Alex (power user, scanning fast)**: greps for `FAIL`/`ERR` in a re-run and gets burned by the missing chain-skip lines — assumes a step silently got dropped from the pipeline, has to re-run with `--force` just to confirm it's actually fine. Also has no `--json` output to script against despite this being exactly the kind of tool a power user wants to pipe into other tooling.

**Jordan (first-timer, no docs open)**: running without `--job` on a multi-job pipeline actually recovers well (`--job is required ... (available: [...])`), but hits the double-printed-error pattern on their very first typo and reasonably wonders if something is actually broken twice. Discovers that caching/skipping exists at all only by accident, since nothing in `--help` mentions it.

**Sam (screen reader / non-visual terminal client)**: hits the ANSI-color-when-piped issue hardest of all three personas — this is the one persona PRODUCT.md explicitly commits to supporting, and it's the one currently least served: every redirected or captured line carries literal escape-code noise that a screen reader will read character-by-character or a log parser will choke on.

## Minor Observations

- `failing`/`exhaust` in `examples/flow.yml` intentionally exit 0 because their own `assert.execution` matches the recorded log (confirmed in `internal/pipeline/pipeline.go`'s `checkExecution` — this is a deliberate self-test feature, not a general "steps run swallows failures" bug). Still, a first-time reader watching only the transcript (not the YAML) sees `job failed` printed and the process exit cleanly, with nothing in the output itself flagging "this failure was expected and asserted."
- YAML parse errors give a line number but no column or snippet of the offending text (`yaml: line 39: found a tab character that violates indentation`) — one step short of pointing at the actual character.
- Debug-level output is rich and well-organized when you ask for it; the gap is entirely about the default level's edge cases, not a lack of underlying diagnostic capability.

## Questions to Consider

- Is a `--json`/`--quiet` machine-readable output mode ever in scope for `steps watch` running under systemd/CI, where today's human-formatted `tint` lines are the wrong shape entirely?
- Given PRODUCT.md already commits to non-visual/screen-reader usability, was the unconditional-color behavior a known gap or simply unnoticed until now?
- Should a job whose failure was cleared by `assert.execution` say so explicitly in its own transcript, not just via a clean exit code a scripted caller would see?
