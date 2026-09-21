package cli

// The daemon's half of `steps mcp login -p`: it runs internal/mcp's flow on its own disk and lets internal/web route the browser's redirect to it. Here rather than in internal/web because depguard keeps internal/mcp out of that package, which is the same split web.Manager draws.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// loginBound is how long a started login waits for its browser. A person reading a consent screen is slow; a login nobody finishes must still let go of its goroutine.
const loginBound = 10 * time.Minute

// logins is every oauth login this daemon has started, by SERVER name: that is what a token file is keyed by (mcp.TokenPath), so two logins for one name would be racing to write one file, and the second replaces the first instead.
type logins struct {
	base context.Context //nolint:containedctx // a login outlives the request that started it, and dies with the daemon
	mu   sync.Mutex
	held map[string]*pendingLogin
	// stale is the logins a newer one replaced, newest last, kept only so the browser still at a consent screen can be told what happened; see staleLogins.
	stale []staleLogin
	wait  sync.WaitGroup
	// attempts numbers the logins this daemon has started, which is what tells one attempt from the one that replaced it under the same server name.
	attempts atomic.Uint64
}

type pendingLogin struct {
	hosted *stepsmcp.HostedCallback
	cancel context.CancelFunc
	mu     sync.Mutex
	status web.LoginStatus
}

// staleLogin is a replaced login reduced to the two things its abandoned browser still needs: the state its redirect will carry, and the page to put it back on.
type staleLogin struct {
	state string
	back  string
}

// staleLogins is how many replaced logins stay answerable. A login a PAGE started has somebody at a consent screen: replace it and their redirect matches nothing, so they finish authorizing and land on a bare 404 for a thing that did happen — somebody else started the same login. Keeping the state matchable long enough to say so costs a handful of pointers, and the cap is what stops it being a leak.
const staleLogins = 4

func newLogins(base context.Context) *logins {
	return &logins{base: base, held: map[string]*pendingLogin{}}
}

// nextID names one ATTEMPT, since the map is keyed by server name and the newest login owns that name. A counter rather than a nonce: it is compared with itself by the request that started the login and never leaves this process as anything a caller could authenticate with.
func (l *logins) nextID() string {
	return strconv.FormatUint(l.attempts.Add(1), 10)
}

func (p *pendingLogin) set(change func(*web.LoginStatus)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	change(&p.status)
}

func (p *pendingLogin) read() web.LoginStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.status
}

// StartLogin resolves server from the configuration this daemon is SERVING — so the token is bound to the endpoint the pipeline will actually dial, which a login from somebody's local file never promised — and starts the flow without waiting for it: discovery and registration are network calls, and the browser half takes as long as a person does.
func (l *logins) StartLogin(pipeline *web.Pipeline, server string, req web.LoginRequest) (web.LoginStatus, error) {
	srv, err := pipeline.Config().FindMCPServer(server)
	if err != nil {
		return web.LoginStatus{}, fmt.Errorf("%w", err)
	}

	if srv.Auth.Type != "oauth" {
		return web.LoginStatus{}, fmt.Errorf("mcp server %q is not auth: {type: oauth}; nothing to log in to", server)
	}

	redirect, err := redirectFor(req.Base)
	if err != nil {
		return web.LoginStatus{}, err
	}

	back, err := returnTo(req.Return)
	if err != nil {
		return web.LoginStatus{}, err
	}

	ctx, cancel := context.WithTimeout(l.base, loginBound)
	pending := &pendingLogin{cancel: cancel, status: web.LoginStatus{State: web.LoginPending, ID: l.nextID()}}
	pending.hosted = stepsmcp.NewHostedCallback(redirect, back, func(authURL string) {
		pending.set(func(status *web.LoginStatus) { status.AuthorizeURL = authURL })
	})

	l.mu.Lock()
	if previous := l.held[server]; previous != nil {
		previous.cancel()
		l.keepStale(previous)
	}

	l.held[server] = pending
	l.mu.Unlock()

	l.wait.Go(func() {
		defer cancel()

		loginErr := stepsmcp.LoginHosted(ctx, *srv, pending.hosted)

		pending.set(func(status *web.LoginStatus) {
			// The path even on failure: a login that authorized and then refused itself (no refresh token) DID save what it got, and says so.
			status.TokenPath, _ = stepsmcp.TokenPath(server)

			if loginErr != nil {
				status.State, status.Message = web.LoginFailed, loginErr.Error()

				return
			}

			status.State = web.LoginAuthorized
		})
	})

	return pending.read(), nil
}

func (l *logins) LoginStatus(server string) (web.LoginStatus, bool) {
	l.mu.Lock()
	pending := l.held[server]
	l.mu.Unlock()

	if pending == nil {
		return web.LoginStatus{}, false
	}

	return pending.read(), true
}

func (l *logins) LoginCallback(state string) http.Handler {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, pending := range l.held {
		if pending.hosted.Matches(state) {
			return pending.hosted
		}
	}

	return l.replacedCallback(state)
}

// keepStale remembers a replaced login, newest last, dropping the oldest past the cap. Called with the lock held.
//
// Two logins are deliberately not kept. One a TERMINAL started: its redirect answers to the CLI polling for it, and the 404 is what says the state died with the login. And one replaced before it ever produced an authorization URL: nothing was ever handed to a browser, so there is no consent screen anybody is sitting at.
func (l *logins) keepStale(previous *pendingLogin) {
	back := previous.hosted.ReturnsTo()

	state := stateOf(previous.read().AuthorizeURL)
	if back == "" || state == "" {
		return
	}

	l.stale = append(l.stale, staleLogin{state: state, back: back})
	if len(l.stale) > staleLogins {
		l.stale = l.stale[len(l.stale)-staleLogins:]
	}
}

// stateOf reads the state parameter back out of an authorization URL, which is where the flow put the only thing that identifies its redirect.
func stateOf(authURL string) string {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return ""
	}

	return parsed.Query().Get("state")
}

// replacedCallback answers the browser of a login somebody else replaced: back to the page it started from, which reads the status of the login that WON. Called with the lock held. Nothing is consumed and no code is exchanged — this flow is over, and the authorization it is carrying belongs to a login that no longer exists.
func (l *logins) replacedCallback(state string) http.Handler {
	if state == "" {
		return nil
	}

	for _, previous := range l.stale {
		if previous.state != state {
			continue
		}

		back := previous.back

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, back, http.StatusSeeOther)
		})
	}

	return nil
}

// stop is Close under a name the daemon embedding this does not already have. The cancel is what TestCloseTakesAWaitingLoginDownWithIt can see; the Wait is a timing-equivalent mutant no test fails on deterministically — a cancelled login unwinds in microseconds — and is kept because "usually gone by then" is exactly what flakes goleak on a loaded machine.
func (l *logins) stop() {
	l.mu.Lock()
	for _, pending := range l.held {
		pending.cancel()
	}
	l.mu.Unlock()

	l.wait.Wait()
}

// returnTo vets where a finished login may send the browser. A PATH on this daemon and nothing else: the value travels from a request into a Location header, so anything carrying a scheme or a host would make this daemon an open redirector — somebody else's login URL, ending on somebody else's page. Empty is a terminal login, which has no page to return to.
func returnTo(back string) (string, error) {
	if back == "" {
		return "", nil
	}

	// "//host" and "/\host" are both an AUTHORITY to a browser, never a path: the WHATWG parser folds the backslash into a slash for http(s), so Location: /\evil.example navigates off this daemon — while url.Parse reads it as a path with an empty Host and would wave it through below.
	if !strings.HasPrefix(back, "/") || strings.HasPrefix(back, "//") || strings.HasPrefix(back, `/\`) {
		return "", fmt.Errorf("a login returns to a path on this daemon, got %q", back)
	}

	parsed, err := url.Parse(back)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" {
		return "", fmt.Errorf("a login returns to a path on this daemon, got %q", back)
	}

	return back, nil
}

// redirectFor builds the redirect URI from the address the CLI reached this daemon on. Userinfo is REFUSED rather than stripped: the URI is sent to the authorization server and kept by it, so a password on it has already gone somewhere it cannot be taken back from, and a client that sent one has a bug worth hearing about.
func redirectFor(base string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("a login needs the http(s) address this daemon was reached on, got %q", base)
	}

	if parsed.User != nil {
		return "", errors.New("the daemon address sent for the redirect carries credentials, which would be handed to the authorization server")
	}

	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("the daemon address %q carries a query or fragment, which a redirect URI cannot", base)
	}

	return strings.TrimSuffix(parsed.String(), "/") + web.MCPCallbackPath, nil
}
