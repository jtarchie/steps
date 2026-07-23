# 007 — Webhook-triggered checks

**Source:** [robustness.md #8](../robustness.md#8-webhook-triggered-checks)
**Tier:** 1 (Concourse gap) · **Priority rationale:** lowest urgency of the
Tier 1 set — polling already works today, this is a latency/rate-limit
optimization on top of it, not a correctness gap. Ships last among the
Concourse-parity features.

## Feature

Let an external system (e.g. a git host) notify `steps watch` to check a
specific resource immediately, instead of waiting on the poll interval.

```yaml
resources:
  - name: repo
    type: git-like
    source: {uri: ...}
    webhook_token: ((hook_token))
# steps watch pipeline.yml --listen :8080
# POST /check/repo?token=... => immediate check of that resource
```

## Additional feature details

- **The token is a credential, not config.** `webhook_token:` should follow
  the same pattern as `api_key_env` — sourced from an env var reference, not
  a literal in YAML — otherwise it lands in `state.db` in cleartext via the
  merkle content map, the exact class of problem the repo's trust-boundary
  rules already exist to prevent for `endpoint:`/`api_key_env`.
- **Opt-in, off by default.** The listener should only bind when
  `--listen` is explicitly passed — `steps watch` today has no open network
  port, and that should remain true unless a user asks for one.
- **Untrusted body, trusted doorbell only.** The webhook payload's *content*
  must never be parsed or trusted as data (commit SHA, branch name, etc.) —
  its only effect should be "run `pollOnce` for this resource now." This
  keeps the trust boundary trivial: worst case, a bad actor with the token
  triggers extra checks, not injects fake version data.
- **Debounce/coalesce.** A host that fires many webhooks in a burst (e.g. a
  force-push triggering several event types) shouldn't queue N redundant
  checks — in-flight or very-recent checks for the same resource should
  coalesce into one.
- **Observability.** Needs a log line per received webhook (source IP,
  resource, accepted/rejected by token) and ideally a lightweight health
  endpoint so operators can confirm the listener is actually up before
  wiring a webhook to it.
