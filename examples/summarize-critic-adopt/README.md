# summarize-critic-adopt

The `adopt: self` variant of [`../summarize-critic/`](../summarize-critic/README.md).
Same machine shape, same critic, same guards, same mock script — one change: the
drafter **continues its own conversation** across revisions (context rung 3) instead of
being re-primed from scratch with distilled feedback (rung 1). Running both examples
A/B-tests the two context philosophies from
[DESIGN.md § Context between states](../../DESIGN.md).

See the sibling README for the full feature table and prerequisites; this one covers
the delta.

## What differs

| | `summarize-critic` (rung 1: `ctx`) | this example (rung 3: `adopt: "self"`) |
|---|---|---|
| Revision context | fresh conversation; article + distilled `ctx.critique.issues` re-sent | own prior conversation replayed; only the feedback appended |
| Article tokens | re-sent on every revision | sent once, on the first visit |
| Transcript size | constant per visit | accumulates across visits (reasoning-channel messages are stripped from replay; `adopt: {from: "self", lastTurns: N}` trims further) |
| Behavior | clean slate — no anchoring to prior phrasing | working memory — remembers its own reasoning, may anchor to it |
| Critic | fresh every round | fresh every round (deliberately **not** adopted — judges must not drift) |

Rules of thumb this pair demonstrates: prefer `adopt` when the source material is large
relative to the feedback (token savings compound per revision) or when revision
genuinely benefits from the agent's own working memory; prefer `ctx` re-priming when
you want maximal independence between attempts and cleaner reproducibility. The critic
staying hermetic in *both* variants is the design point that doesn't change.

## Run it

```sh
# Deterministic (CI) — fixture shared with the sibling example
steps run workflow.js \
  --input article=@../summarize-critic/fixtures/article.txt \
  --mock mock_responses.yaml

# Live local iteration
steps run workflow.js --input article=@../summarize-critic/fixtures/article.txt

# Validate (adopt: self must pass the graph-predecessor check)
steps validate workflow.js --print
```

## Expected mock trace (what CI asserts)

Routing is identical to the sibling — same 5 transitions, `visits.draft == 2`, same
retries on `critique`, `out/summary.md` written. What differs, and what CI asserts on
top of the sibling's assertions:

- Draft visit 2's conversation *contains* visit 1's messages (the replayed transcript)
  followed by the feedback message — asserted from the `handler_finished` event's
  journaled `messages`.
- The article text appears **exactly once** across the drafter's entire conversation
  (it is never re-sent), while in the sibling it appears once per visit.
- The feedback message on visit 2 contains only the critique issues — no ARTICLE block
  (the `ctx.critique ? ... : ...` branch).
- Token accounting: adopted transcript tokens count toward the state's usage in
  `handler_finished` (budget rules from DESIGN.md apply).

Additionally exercised here beyond the sibling: journal replay of normalized messages
(`adopt` reads the prior execution from the journal, so it must survive a kill-and-
`steps resume` between visits), and the `adopt: self` first-visit case (no prior
conversation → fresh start, which for `self` is the documented, non-error behavior).
