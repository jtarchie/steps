# 011 — Approval steps: a human in the plan

**Source:** [robustness.md #12](../robustness.md#12-approval-steps--a-human-in-the-plan)
**Tier:** 2 (agent-native) · **Priority rationale:** agent pipelines
eventually gate something irreversible; this is the oldest and most direct
safeguard for that, but it's lower priority than budgets since most pipelines
can launch and prove value without a human-gate step first.

## Feature

Pause the plan for explicit human sign-off before an irreversible step (e.g.
a `put` that publishes).

```yaml
- approval:
    message: "Draft is in draft/summary.md — publish?"
    timeout: 24h
- put: blog
```

## Additional feature details

- **Who can approve — explicitly scoped for v1.** The natural first cut is
  "anyone with access to the `steps` CLI/host" — no separate identity system.
  This should be stated as a deliberate initial scope limitation, not an
  oversight, since it's the kind of gap someone will ask about ("can
  anyone approve?") the moment this ships.
- **Notification is a prerequisite, not a nice-to-have.** An approval step
  that just silently parks with no one told is useless in practice — this
  needs to tie into *some* outbound notification (a hook firing a `put` to
  Slack/email is the natural fit given hooks already exist) so a human
  actually learns there's something to approve.
- **Audit trail.** Who approved, when, and (if rejected) why should be
  recorded in the store — this is exactly the kind of decision that later
  needs to be reconstructed ("who signed off on this deploy") and shouldn't
  rely on external chat history alone.
- **Timeout behavior.** An expired approval should classify as `aborted`
  (the outcome vocabulary already distinguishes this from `failed`) and
  ideally fire a distinct notification ("approval for X expired unanswered")
  rather than failing silently into the same bucket as a rejected approval.
- **CLI surface.** Needs `steps approvals` (list pending) and
  `steps approve <job> <id>` / `steps reject <job> <id>` — under `steps
  watch` the pending approval is a queue state, so this reuses the existing
  trigger-queue persistence rather than inventing new storage.
- **Interaction with webhook checks ([007](007-webhook-triggered-checks.md)).**
  A webhook listener, if that feature ships, gives a natural path to remote
  approval (`POST /approve/<id>?token=...`) instead of requiring CLI/host
  access — worth noting as a synergy even though the two features are
  otherwise independent.
