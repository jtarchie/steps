package cli

// The daemon's half of `steps mcp login -p`: it runs internal/mcp's flow on its own disk and lets internal/web route the browser's redirect to it. Here rather than in internal/web because depguard keeps internal/mcp out of that package, which is the same split web.Manager draws.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
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
	wait sync.WaitGroup
}

type pendingLogin struct {
	hosted *stepsmcp.HostedCallback
	cancel context.CancelFunc
	mu     sync.Mutex
	status web.LoginStatus
}

func newLogins(base context.Context) *logins {
	return &logins{base: base, held: map[string]*pendingLogin{}}
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

	ctx, cancel := context.WithTimeout(l.base, loginBound)
	pending := &pendingLogin{cancel: cancel, status: web.LoginStatus{State: web.LoginPending}}
	pending.hosted = stepsmcp.NewHostedCallback(redirect, func(authURL string) {
		pending.set(func(status *web.LoginStatus) { status.AuthorizeURL = authURL })
	})

	l.mu.Lock()
	if previous := l.held[server]; previous != nil {
		previous.cancel()
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
