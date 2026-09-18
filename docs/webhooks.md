# Webhooks

`type: webhook` is a built-in resource type whose versions are **deliveries**: each HTTP POST a service sends to `steps web` becomes a version of the resource, and a job that gets it reads what the service sent. There is no check. The delivery is the data.

That makes two things possible that polling cannot do:

- **Start a job from anything that sends webhooks**, including services with nothing to poll — a new Sentry issue, a PagerDuty incident, a Linear ticket, a Stripe payment.
- **Build exactly what the sender reported** — the commit a push names, read from its payload — rather than whatever the next poll of the repository happens to find.

```yaml deliver=github-push
resources:
- name: push
  type: webhook
  source:
    provider: github
    secret_env: GITHUB_WEBHOOK_SECRET   # the variable NAME, never the secret
    filter: 'event == "push" && payload.ref == "refs/heads/main"'
    id: 'payload.after'
    version:
      branch: 'trimPrefix(payload.ref, "refs/heads/")'

jobs:
- name: build
  plan:
  - get: push
    trigger: true
  - task: checkout
    inputs: [push]
    # A real pipeline clones the commit the push names:
    #   git clone "$(jq -r .repository.clone_url push/body)" src
    #   git -C src checkout "$(jq -r .after push/body)"
    run: cat push/version.json
    assert:
      stdout: '"id":"6113728f27ae82c7b1a177c8d03f9e96e0adf246"'
  assert:
    execution: [push, checkout]
    outcome: succeeded
```

The delivery's URL is `POST /p/<pipeline>/hooks/<resource>` on the daemon — here, `/p/app/hooks/push` for a pipeline set as `app`.

## What a get writes

| File | What it holds |
|---|---|
| `body` | The request body, byte for byte. Usually JSON; Slack's slash commands send form-encoded data, which is why this is not `body.json`. |
| `headers.json` | The request headers as a JSON object, names lower-cased, first value of each. `Authorization`, `Cookie` and the provider's signature header are removed: the `token` provider carries the secret itself. |
| `version.json` | The version, as below. |

## The version

Every version is `{id, event, ...}`:

- **`id`** is the sender's own delivery id when it sends one (`X-GitHub-Delivery`, `webhook-id`, Stripe's `evt_…`), so a **redelivery lands on the version already recorded and builds nothing** — GitHub's "Redeliver" button and a sender's automatic retry are both safe. A sender with no id gets a random one. `source.id` replaces it: `id: 'payload.after'` above makes two deliveries of one commit one version.
- **`event`** is what the sender says happened (`X-GitHub-Event`, Stripe's `type`, Sentry's resource), left out when it says nothing.
- **`source.version`** adds fields, each an expression. They are what the web UI shows for the version, so something better than a GUID is worth one line. `id` and `event` cannot be set here.

> **The default version mode is `latest`, as for every resource.** A burst of deliveries builds only the newest one; the rest are recorded and skipped over. If every delivery matters — each Sentry issue triaged, each payment handled — say so on the get:
>
> ```yaml fragment
> - get: issue
>   trigger: true
>   version: every
> ```
>
> This is deliberately not a different default for this type: a get behaving differently depending on what type its resource is would be a rule to remember, and `version: every` is one line.

## Providers

`provider:` is **declared, never detected**. A sender controls its own headers, so guessing the scheme from them would let a delivery pick the check it has to pass. An unknown provider is refused when the pipeline is set.

| `provider:` | Checks | Delivery id | `event` |
|---|---|---|---|
| `github` | `X-Hub-Signature-256` (HMAC-SHA256 of the body) | `X-GitHub-Delivery` | `X-GitHub-Event` |
| `gitlab` | `X-Gitlab-Token` equals the secret | `X-Gitlab-Event-UUID` | `X-Gitlab-Event` |
| `gitea` | `X-Gitea-Signature` | `X-Gitea-Delivery` | `X-Gitea-Event` |
| `forgejo` | `X-Forgejo-Signature` | `X-Forgejo-Delivery` | `X-Forgejo-Event` |
| `bitbucket` | `X-Hub-Signature` (`sha256=`) | `X-Request-UUID` | `X-Event-Key` |
| `slack` | `X-Slack-Signature` over the timestamp and body; answers `url_verification` | `event_id` | `type`, or `event.type` for an `event_callback` |
| `stripe` | `Stripe-Signature` (`t=`, `v1=`) | `id` | `type` |
| `sentry` | `Sentry-Hook-Signature` | `Request-ID` | `Sentry-Hook-Resource` |
| `linear` | `Linear-Signature` | `Linear-Delivery` | `Linear-Event` |
| `pagerduty` | `X-PagerDuty-Signature` (any `v1=`) | `event.id` | `event.event_type` |
| `honeybadger` | `Honeybadger-Token` equals the secret | — | `event` |
| `standard-webhooks` | [Standard Webhooks](https://www.standardwebhooks.com/) (Svix, Clerk, Resend, OpenAI…); the `whsec_` secret is decoded | `webhook-id` | `type` |
| `token` | `Authorization: Bearer <secret>` | — | — |
| `custom` | whatever `source.signature` describes — see below | — | — |

Rules that hold for every provider:

- **An empty secret refuses every delivery.** While `secret_env` is unset or empty, nothing gets in; "no secret, no check" would turn a deployment mistake into an open endpoint.
- **Timestamped schemes have a five-minute tolerance**, both ways (Slack, Stripe, Standard Webhooks, and `custom` with a `timestamp:`). Without it a captured delivery replays forever.
- **Comparison is constant-time**, and the body is read under a cap: 1 MiB unless `source.max_body` raises it. A bigger body is answered 413 — GitHub allows up to 25 MB, so a pipeline receiving large pushes raises the cap.

Not supported, and why: Meta, Microsoft Graph and Dropbox verify with a GET, and this route is POST only; Twilio signs the full URL, which a tunnel rewrites; Amazon SNS signs with certificates rather than a shared secret.

## Filtering

`source.filter` is a boolean expression. A delivery it answers false for is acknowledged (2xx, so the sender logs success) and recorded nowhere — GitHub's setup `ping`, pushes to other branches, PR comments. It is the only filtering mechanism; `event == "push"` is how to say "only pushes".

## Expressions

`filter`, `id`, each `version` field, and `custom`'s `signed`/`timestamp` are [expr](https://expr-lang.org/) expressions over the delivery:

| Name | What it is |
|---|---|
| `provider` | The declared provider. |
| `event` | As in the version. |
| `method` | `POST`. |
| `headers` | Request headers, names lower-cased: `headers["x-github-event"]`. |
| `query` | Query-string parameters, first value of each. |
| `body` | The raw body, as a string. |
| `payload` | The body decoded as JSON, or nil when it is not JSON. `payload?.ref` reads a field that may be absent. |

They are **pure**: no `env()`, `http()`, `file()` or `fail()` — the functions an [`expr:` resource type](expr.md) has — and no clock. They are compiled when the pipeline is set, so a syntax error is refused by `steps pipeline set` on the daemon, not discovered at the first delivery.

The order is **verify → handshake → filter → id → version → record**, so a filter never sees a delivery whose signature failed. An expression that fails at evaluation time — `payload.head.sha` on a payload with no `head` — answers **500 and records nothing**, logged as `webhook.expr_error`: the sender shows a failed delivery, which can be redelivered once the pipeline is fixed.

## Custom schemes

A sender not in the table, signing with an HMAC, is described rather than coded:

```yaml deliver=custom-signed
resources:
- name: deploys
  type: webhook
  source:
    provider: custom
    secret_env: DEPLOY_HOOK_SECRET
    max_body: 65536
    signature:
      header: X-Signature                # where the signature arrives
      prefix: "v1="                      # text before it in that header
      encoding: base64                   # hex (default) or base64
      algorithm: sha512                  # sha256 (default), sha1 or sha512
      signed: 'headers["x-timestamp"] + "." + body'   # default: the raw body
      timestamp: 'headers["x-timestamp"]'             # unix seconds, within five minutes

jobs:
- name: announce
  plan:
  - get: deploys
    trigger: true
  - task: show
    inputs: [deploys]
    run: cat deploys/body
    assert:
      stdout: '"status":"finished"'
  assert:
    execution: [deploys, show]
    outcome: succeeded
```

`signed` and `timestamp` only **build strings**. The HMAC and the comparison happen in Go, the same code the built-in providers use, so an expression can never be the thing that decides a signature is valid. A `timestamp:` protects against replay only if `signed` includes it, as above.

## Receiving deliveries

The route keeps the daemon's rules for anything a machine rather than a browser sends:

- **POST only.**
- **A bad signature and an unknown resource are both 401**, so the endpoint is not a directory of a pipeline's resource names. A pipeline with no webhook resource at all answers 404.
- **Exempt from `--read-only` and from the UI's same-origin check.** It is authenticated by the sender's signature, not by the UI's (absent) authentication, and a read-only build box that could not be notified is most of what a read-only build box is for. To turn it off, give the pipeline no webhook resource.
- **Recorded before it is answered.** The version, its payload and the jobs it triggers are written in one transaction, and only then does the sender get a 2xx. A failed write answers 500, so the sender retries rather than the event being lost.
- **A paused pipeline records the delivery and queues nothing.** The first poll after [`steps pipeline unpause`](web.md) builds it. Answering 2xx and dropping it would be a loss the sender could never redeliver, since it believes it succeeded.

### Exposing it

steps runs on your own machine, so a sender reaches it through a tunnel. Forward **only** the hook paths: the UI has no authentication (see [web.md](web.md#security)). With [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/), an ingress rule with a path filter does it:

```bash
# ~/.cloudflared/config.yml
tunnel: <tunnel-id>
credentials-file: /home/me/.cloudflared/<tunnel-id>.json
ingress:
  - hostname: hooks.example.com
    path: ^/p/[^/]+/hooks/[^/]+$
    service: http://localhost:8080
  - service: http_status:404
```

Tailscale Funnel and any reverse proxy work the same way: route `^/p/[^/]+/hooks/[^/]+$` and nothing else.

## Lost deliveries

**A delivery that never arrives is never built.** There is no polling fallback, deliberately: a fallback needs a check, and a check is exactly what this type does not have — a second source of versions that could disagree with the first. When the daemon was down or the tunnel dropped a request, the recovery is the sender's own redelivery (GitHub, Stripe and Svix each have a button for it), and a redelivery of something already recorded builds nothing.

## Stored payloads

Payloads are kept in the state database, in cleartext, next to the version they belong to — because a job that runs later, or a rerun, has to read them. **They can contain personal data**: Stripe customers, Sentry stack traces, email addresses in a Linear ticket. They are bounded only by [`defaults.version_history:`](resources.md): a version pruned from history takes its payload with it. A pipeline receiving sensitive deliveries should set that low.

## Running locally

`steps run` and `steps test` have no daemon to POST to, so `--deliver` hands them a captured request:

```bash
steps run pipeline.yml --deliver push=push.http
```

The file is a raw HTTP request — request line, headers, a blank line, the body — which is what GitHub's "Recent Deliveries" panel and `curl -v` show:

```text
POST /p/app/hooks/push HTTP/1.1
X-GitHub-Event: push
X-GitHub-Delivery: 72d3162e-cc78-11e3-81ab-4c9367dc0958

{"ref":"refs/heads/main","after":"6113728f27ae82c7b1a177c8d03f9e96e0adf246"}
```

It goes through the pipeline's own `filter`, `id` and `version` — where a pipeline's mistakes live — and skips only the signature, which a local command holds no secret for. A delivery the filter rejects is an error rather than a silent rebuild of whatever was recorded before.

## Compared with Concourse

Concourse's webhook is a doorbell: `POST …/check/webhook?webhook_token=…` triggers a check and ignores the payload entirely, and Concourse declined payload handling on purpose ([concourse#2240](https://github.com/concourse/concourse/issues/2240)) — every resource type would have to parse every service's payload, and a version only a webhook had seen would disagree with what polling finds. Here only the `webhook` type knows providers, so the first cost does not arise, and the second is the lost-delivery rule above, accepted and stated. See [conformance.md](conformance.md).
