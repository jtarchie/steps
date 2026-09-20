# Authentication

`steps web` asks for nothing by default, and on the loopback address it binds by default that is the right answer: the UI and the terminal that started it are the same trust domain. The moment the daemon is reachable from somewhere else, it is not — **a pipeline is arbitrary commands, so `steps pipeline set` is a remote shell** ([web.md, Security](web.md#security)).

HTTP Basic is what closes that, and it is the whole of the authentication here. No OAuth, no login command, no sessions, no per-pipeline permissions — one username and one password, held by the daemon, carried by the CLI.

## Turning it on

```bash
steps web \
  --basic-auth-username ops \
  --basic-auth-password "$(openssl rand -base64 24)"
```

| Flag | Environment variable |
|---|---|
| `--basic-auth-username` | `STEPS_BASIC_AUTH_USERNAME` |
| `--basic-auth-password` | `STEPS_BASIC_AUTH_PASSWORD` |

Prefer the environment variables in a deployment: a password on a command line is in `ps` output and in shell history.

**Both or neither.** A daemon given only one of them refuses to start, rather than serving open access to somebody who believes it is protected.

**Unset is open access**, which is what every existing local workflow is. `steps web` on a routable address with no credentials prints a warning once at startup naming the two flags; it does not refuse, because an operator who has put the daemon behind a tunnel or a reverse proxy has already answered the question.

## What it covers

Every route: the pages, the static assets, `/docs`, the live-run SSE streams, and the `/api` endpoints `steps pipeline` and `steps runs abort` talk to. A browser gets a `WWW-Authenticate: basic realm="steps"` challenge, so it prompts.

**One route is exempt: `POST /p/<pipeline>/hooks/<resource>`**, a [webhook](webhooks.md) delivery. A sender has no credentials to give and authenticates with its own signature instead — the same reason that route is exempt from the same-origin check and is not withheld by `--read-only`. A delivery with a bad signature is still refused by the hook, with a 401 of its own.

Basic auth does not replace the `/api` browser refusal described in [web.md](web.md#security): a request carrying `Origin` or a browser-shaped `Sec-Fetch-Site` is still refused whatever credentials it has, because a page that talked an operator into typing a password must still not be able to set a pipeline.

## From the CLI

Every command that talks to a daemon takes `--target` (or `STEPS_TARGET`). Credentials go in that URL's userinfo:

```bash
steps pipeline set -c pipeline.yml --target https://ops:PASSWORD@steps.example.com
export STEPS_TARGET="https://ops:PASSWORD@steps.example.com"
steps pipeline list
```

A password holding a `@`, a `:` or a `/` has to be percent-encoded, as in any URL.

They travel in an `Authorization` header, never in the request URL, and the userinfo is stripped from every message the CLI prints — a refusal, a timeout, an unreachable daemon — so a password that reached a terminal, a CI log or a pasted bug report would be a defect.

A request with no credentials where credentials are required says what to do about it rather than repeating `Unauthorized`:

```
$ steps pipeline list
steps: error: https://steps.example.com asked for credentials it did not get —
put them in the target URL: --target http://user:password@host (or STEPS_TARGET)
```

## What it is not

- **Not a user model.** One credential pair, shared by everyone who has it. Nothing records *who* triggered a job, and nothing can be permitted to one person and withheld from another.
- **Not a substitute for TLS.** Basic auth sends the password on every request, base64-encoded and not encrypted. Over plain HTTP anything on the path reads it. Terminate TLS in front of the daemon — a platform's own router, a reverse proxy, a tunnel.
- **Not rate limited.** There is no lockout and no delay on a wrong password. Use a generated password, not one somebody chose.
- **Not a reason to expose the daemon.** The set endpoint is still a remote shell; authentication decides *who* reaches it, not *what* they can do once they have.
