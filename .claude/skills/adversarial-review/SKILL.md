---
name: adversarial-review
description: Deep adversarial verification of a body of recent work in this repo — parallel reviewers with distinct lenses, sabotage-proofing of test coverage, and a docs-vs-code honesty audit. Use after landing a large feature arc or a batch of review fixes, when the user asks to "double check everything", or before declaring a subsystem done. Heavier than /code-review: it verifies fixes actually fix, proves tests actually catch, and checks prose claims against code.
user-invocable: true
argument-hint: "[git range or subsystem, e.g. 276a3b1..HEAD or internal/venue]"
---

# Adversarial review

This skill encodes a review structure that, run against this repo's venue/eviction work (2026-08-25), surfaced ~25 confirmed defects that two prior `/code-review` passes had missed — including money bugs (stranded EC2 instances), liveness holes (uncancellable wedged workers), and an entire untested layer whose deletion shipped green. It also encodes the failure modes of the review itself, which matter as much as the structure.

## Structure: parallel reviewers, distinct lenses

Spawn 4–6 background agents in ONE message, each scoped to a lens, not a file list. Lenses that earned their keep:

1. **Fix verification** — when reviewing fixes: for each claimed fix, verify it fixes the finding *completely* and introduced nothing. Instruct: report VERIFIED-FIXED / INCOMPLETE (with residual scenario) / REGRESSION.
2. **Concurrency and liveness** — locks held across I/O, cancellation reaching blocked reads AND writes, teardown while peers are connected, goroutine lifetimes. Instruct the agent to *enumerate everything reachable under each held lock*.
3. **Money/resource paths** — anything that acquires what costs money or leaks: every acquire needs a release on every path including cancellation; cleanup must not use the context whose cancellation caused the cleanup; releases must be findable after partial failures. Ask "who pays, and does any path forget."
4. **Reference conformance** — for protocol ports or spec implementations: field-by-field against the authoritative source, NOT the port's own tests (a fake written from the same understanding shares its misreadings). Fetch the real peer's source if it exists.
5. **Test vacuity** — sabotage-based: revert each fix or guard in the working copy, run the specific test, require failure, restore. See the protocol below — this lens has sharp edges.
6. **Docs honesty** — every behavioral claim in touched docs/comments checked against code as it NOW stands; prose drifts within a single session.

Require from every agent: findings only with file:line and a concrete failure scenario; an explicit list of threads *considered and refuted*, with why. The refutations are half the value — they are what lets the orchestrator skip re-verifying.

## The sabotage protocol (scars included)

A test only counts if it fails when the thing it pins is broken. Two tests written in that very session were vacuous against their own bug — one needed its fixture rewritten twice before it could even *reproduce* the failure (a single-frame stub couldn't desync what only multi-frame streams desync; a 16-byte garbage payload couldn't fail a tar reader that blocks awaiting a 512-byte header). Sabotage finds this; nothing else did.

Rules, each paid for:

- **Sabotage via in-memory edit and rewrite (python/perl), never `git checkout --` restore, when the fix itself is uncommitted.** `git checkout -- file` restores HEAD and silently deletes the uncommitted fix along with the sabotage. This happened; the "fixed" test then failed mysteriously against code that no longer contained the fix.
- Only ONE agent may mutate the working tree at a time, and other agents must be told the tree may shift under them — two reviewers reported the vacuity agent's in-flight sabotage as real regressions.
- The saboteur's final act is `git status --porcelain`, reported, proving restoration.
- A sabotage that *passes* is a finding (GAP: no coverage), not a shrug.

## What the structure cannot do

State this in the final report, because overclaiming is its own defect:

- Reviewers all missed the deepest bug of that session (a resolved worker's URL never reaching the dialer — the launch rung had never worked end to end). It was found by *implementing* the fixes with fresh eyes. Review layers reduce; nothing eliminates. Budget implementation time after the review, not just triage time.
- Conformance lenses verify against sources, not against the live peer. An opt-in real-endpoint test (`STEPS_TEST_AWS_INSTANCE` pattern) is the only settlement.

## After the findings

- Fix in coherent commits grouped by mechanism, not by finding number; each commit message says what broke, why the tests didn't see it, and what now pins it.
- Every fix lands with a test **proven by sabotage before committing**.
- Run `task cover-diff` and `task mutate-pkg PKG=...` on the touched packages; triage what they print.
- Re-run the full `task` sequence per commit, as always.
