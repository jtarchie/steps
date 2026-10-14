# Authentication

`steps web` asks for nothing by default, and on the loopback address it binds by default that is the right answer: the UI and the terminal that started it are the same trust domain. The moment the daemon is reachable from somewhere else, it is not — **a pipeline is arbitrary commands, so `steps pipeline set` is a remote shell** ([web.md, Security](web.md#security)).

Accounts close that: local accounts the daemon keeps, or a GitHub or Google account. The CLI carries a local one as HTTP Basic in its `--target` URL, a browser signs in through a form or with GitHub or Google, each request is recorded under the account that made it, and roles decide what each account may do.

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

### Several people: local users

```bash
htpasswd -nbB alex "$ALEX_PASSWORD" >  users
htpasswd -nbB sam  "$SAM_PASSWORD"  >> users

steps web --local-users-file users
steps web --local-user 'alex:$2y$05$…'      # repeatable; the same line htpasswd writes
```

Each account is a name and a **bcrypt hash** — the daemon never holds the password. A line whose password is not a bcrypt hash is refused at startup, saying how to make one and never repeating what it was given. A name given twice, across both flags or alongside `--basic-auth-username`, is refused too: which password worked would otherwise be an accident of order. Blank lines and `#` comments in the file are skipped.

Everyone signs in the same way — the browser prompts, the CLI carries `name:password@` in `--target` — and what each one does is recorded under their own name: a run they trigger says `by alex (local)` ([web.md](web.md#the-transcript)). The `--basic-auth-*` pair still works, and is one more account beside these.

bcrypt is slow on purpose (tens of milliseconds a check) and a browser sends credentials on every request, so a pair that checked out is remembered for a minute. A changed file reaches a running daemon only on restart.

**Unset is open access**, which is what every existing local workflow is. `steps web` on a routable address with no credentials prints a warning once at startup naming the two flags; it does not refuse, because an operator who has put the daemon behind a tunnel or a reverse proxy has already answered the question.

## Signing in from a browser

A browser that is not signed in is sent to **`/login`**, a form asking for an account and its password, and on to the page it asked for once signed in. The session lasts a week and lives on the daemon — the cookie holds a random token, and the state database holds only a digest of it — so **sign out**, in the menu under your name, ends it everywhere at once: a copied cookie stops working too.

- A wrong name and a wrong password get the same sentence, `couldn't sign you in`, and the name you typed is kept.
- After signing in you are sent only to a page on this daemon; anything else goes to `/`.
- The cookie is `HttpOnly` and `SameSite=Lax`, and `Secure` when the daemon is reached over https (directly, or through a proxy that sets `X-Forwarded-Proto`).
- A page's own refresh is never redirected to the form, or challenged: a session that ends while a page sits open answers `401`, and the page stops refreshing and says `signed out · sign in again` where it lists what needs you, linking back to itself.
- The CLI is unchanged: it sends its account on every request and never sees the form.

## Signing in with GitHub or Google

```bash
export STEPS_GITHUB_CLIENT_SECRET=…        # from the OAuth app you registered
export STEPS_GOOGLE_CLIENT_SECRET=…
steps web --external-url https://steps.example.com \
  --github-client-id Iv1.0123456789abcdef \
  --google-client-id 1234-abc.apps.googleusercontent.com
```

Each client id adds a **Continue with …** button to the sign-in page. Register the daemon with the provider first, giving it `<external-url>/auth/github/callback` or `<external-url>/auth/google/callback` as the redirect address.

- **The secret is read from the environment**, never a flag: `$STEPS_GITHUB_CLIENT_SECRET` and `$STEPS_GOOGLE_CLIENT_SECRET` by default, or the variable `--github-client-secret-env`/`--google-client-secret-env` name. An id without its secret, or with no `--external-url` to send the browser back to, stops the daemon before it listens.
- **Who they are**: `alex (github)` by GitHub login, `sam@acme.dev (google)` by email.
- **What is read about them**, at every sign-in, replacing what was read last time: from GitHub, their organizations and `org/team` teams (the app asks for `read:org`); from Google, their email and, for a Workspace account, its domain. Google says nothing about groups without its admin API, so a rule about a Google account can match its address or domain and nothing else.
- **Local accounts and providers mix.** The password form is drawn only when the daemon keeps local accounts. The CLI still needs a local account: a GitHub or Google sign-in is a browser's.
- A sign-in that comes back with a state this daemon did not hand out, or one already used, is refused.

## Who may do what: roles

```bash
steps web --local-users-file users --github-client-id … \
  --role 'admin=Provider == "local" && UserID == "jt"' \
  --role 'operator="acme/eng" in Groups' \
  --role 'viewer="acme" in Organizations || Email endsWith "@acme.dev"'
```

Three roles, Concourse's, each including the one below it:

| Role | May |
|---|---|
| `viewer` | read every page |
| `operator` | trigger, retry, approve, reject, answer, abort, release, pause and unpause |
| `admin` | set, rename and destroy pipelines, connect and test MCP servers, and read the [activity log](web.md#activity) |

Each `--role` is a rule about the signed-in person, an [expr](https://expr-lang.org) expression over `Provider` (`local`, `github`, `google`), `UserID` (the account name, GitHub login or Google address), `Email`, `Organizations` and `Groups`. A person has the highest role whose rule holds.

- **No `--role` at all** keeps today's behaviour: everyone signed in may do everything.
- **With any `--role`, an `admin` rule is required**, or nobody could set a pipeline and the daemon would be stuck as it stands. A `viewer` rule left out lets anyone signed in read; `operator` left out means only admins act.
- **A rule that does not compile stops the daemon** before it listens, naming the role.
- **It is enforced by the daemon, on every request**, not by hiding buttons. A viewer's pages leave the controls out and say once, in the action bar, what the operator role would add; a request for something their role does not cover is refused with `403`, from a browser or from the CLI alike.
- **Rules read the daemon's own record of the person**, written at their last sign-in — never anything the browser carries. That is what keeps an exclusion honest: `operator=!("acme/contractors" in Groups)` reads the groups GitHub reported, so a contractor cannot pass by arriving with none. Leaving a team takes effect at the next sign-in, within a week.
- `--read-only` still withholds the controls from everyone, whatever their role.

## What it covers

Every route: the pages, `/docs`, the live-run SSE streams, and the `/api` endpoints `steps pipeline`, `steps runs abort` and `steps mcp login -p` talk to. A request that is not a browser asking for a page gets a `WWW-Authenticate: basic realm="steps"` challenge. `/login` and the stylesheet and scripts it is drawn with answer anyone: nobody can sign in through a page that needs them signed in first.

**One route is exempt: `POST /p/<pipeline>/hooks/<resource>`**, a [webhook](webhooks.md) delivery. A sender has no credentials to give and authenticates with its own signature instead — the same reason that route is exempt from the same-origin check and is not withheld by `--read-only`. A delivery with a bad signature is still refused by the hook, with a 401 of its own.

**A second route is exempt for the same reason: `GET /mcp/callback`**, where an authorization server sends the browser to finish a [`steps mcp login` against this daemon](mcp.md#authorizing-a-daemon). A redirect is a navigation a third party caused and carries no credential of yours; it is authenticated by the single-use `state` its login minted, and anything else is a `404`. Starting that login, and reading the URL that carries the state, both need credentials.

**A third answers anyone: `GET /healthz`**, which says `ok` and nothing else, for a platform's health check — which sends no credentials. Liveness is all it reveals, and it is never traced (see [observability.md](observability.md)).

## What a browser may start

`/api` refuses anything browser-shaped, so until the **mcp** tab existed a browser could not begin an oauth login at all — only the CLI could, over `/api`. The tab's **Connect** and **Test** buttons deliberately open a browser-reachable way to do both: `POST /p/<pipeline>/mcp/<server>/connect` starts a login, and `POST /p/<pipeline>/mcp/<server>/test` connects to a declared server. This is a real change in posture and not a side effect, so it is stated here rather than discovered.

What bounds it:

- **Both routes require the daemon's credentials**, like every other page route.
- **`sameOriginMutations` refuses a cross-site POST**, which is what stops another page aiming your browser at these routes.
- **Neither can name a server the pipeline does not already declare.** Adding one takes `steps pipeline set`, which is remote-shell-grade already — so the blast radius is the set of servers somebody with that access chose.
- **`--read-only` withholds both**, while keeping the page: seeing that a server needs a login is a diagnostic, and starting one is a control.
- **Where a finished login sends the browser is a path on this daemon**, refused if it carries a scheme or a host, so the route cannot be used as an open redirector.

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

- **Not a substitute for TLS.** Basic auth sends the password on every request, base64-encoded and not encrypted, and a session cookie is as good as the password while it lasts. Over plain HTTP anything on the path reads it. Terminate TLS in front of the daemon — a platform's own router, a reverse proxy, a tunnel.
- **Not rate limited.** There is no lockout and no delay on a wrong password. Use a generated password, not one somebody chose.
- **Not a reason to expose the daemon.** The set endpoint is still a remote shell; authentication decides *who* reaches it, not *what* they can do once they have.
