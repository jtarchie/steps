package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// This file is the local HTTP server half of `steps mcp login`: binding a
// loopback port, telling the browser redirect apart from anything else that
// reaches it, and handing the authorization code back to whoever is waiting.
// login.go is the OAuth half — discovery, registration, and deciding what the
// authorization server is asked for.
//
// The SDK deliberately does not supply any of this (see docs/mcp.md): opening
// a browser and catching a redirect is caller/process-integration work, which
// is why auth.AuthorizationCodeFetcher is an interface it leaves to the
// caller. fetch, at the bottom, is where the two halves meet.

// loopbackCallback runs a local HTTP server on an ephemeral port to receive
// the OAuth redirect, per auth.AuthorizationCodeFetcher's contract: the SDK
// itself deliberately does not provide this (see docs/mcp.md) — opening a
// browser and catching the redirect is caller/process-integration logic.
type loopbackCallback struct {
	listener    net.Listener
	server      *http.Server
	redirectURL string
	result      chan callbackResult

	// mu guards want, the state of the authorization request currently in
	// flight. handle runs on the server's goroutine and fetch on the login
	// one, so the two need it even though only fetch ever writes.
	mu   sync.Mutex
	want string
}

type callbackResult struct {
	code, state, iss string
	err              error
}

// expect records the state parameter of the request about to be opened, so
// handle can tell the real redirect from anything else that reaches the
// loopback port. The SDK regenerates state on every attempt, so this is set
// per attempt rather than once per listener.
func (cb *loopbackCallback) expect(state string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.want = state
}

// matches reports whether a callback's state is the one currently expected.
// An empty expectation matches nothing: until fetch has opened a URL there is
// no request outstanding, so anything arriving is not ours.
func (cb *loopbackCallback) matches(state string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	return cb.want != "" && state == cb.want
}

// deliver hands a result to whoever is waiting, without ever blocking. The
// channel holds one result and the waiter takes exactly one, so a send that
// would block means a result is already queued — the flow is settled and this
// request is a straggler, not the answer.
func (cb *loopbackCallback) deliver(res callbackResult) bool {
	select {
	case cb.result <- res:
		return true
	default:
		return false
	}
}

// newLoopbackCallback binds the local port the browser redirect comes back
// to. Port 0 — an ephemeral port — is the default because it always binds and
// never collides, which is what RFC 8252 §7.3 asks authorization servers to
// allow for exactly this reason.
//
// The MCP authorization spec asks for the opposite ("authorization servers
// MUST validate exact redirect URIs against pre-registered values"), and a
// server enforcing that cannot be logged into with a port that changes every
// run: nothing registered in a dashboard will ever match. auth.callback_port
// pins it for those, which are largely the same servers auth.client_id exists
// for. A pinned port can be in use, and that error is worth reporting plainly
// rather than falling back to an ephemeral one that produces a redirect URI
// mismatch further along, where the cause is no longer visible.
func newLoopbackCallback(ctx context.Context, port int) (*loopbackCallback, error) {
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		if port != 0 {
			return nil, fmt.Errorf("mcp: listen for oauth callback on auth.callback_port %d: %w", port, err)
		}

		return nil, fmt.Errorf("mcp: listen for oauth callback: %w", err)
	}

	// Read back rather than reused: with port 0 the kernel chose it, and with
	// a pinned port this is the same number, so one path builds the URL.
	bound := listener.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // net.Listen("tcp", ...) always yields a *net.TCPAddr

	cb := &loopbackCallback{
		listener:    listener,
		redirectURL: fmt.Sprintf("http://127.0.0.1:%d/callback", bound),
		result:      make(chan callbackResult, 1),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", cb.handle)
	cb.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = cb.server.Serve(listener) }()

	return cb, nil
}

// handle receives the browser redirect. Two things it must NOT do, because
// the port is on loopback and therefore reachable by anything else on the
// machine (a stale tab, a prefetch, a port scan): treat an unrecognized
// request as the answer, and block on the result channel.
//
// The state check is what makes it the answer — the SDK compares state again
// after we return, but by then a stray request has already been consumed as
// though it were the redirect, and the real one has nowhere to go. Rejecting
// here keeps the flow waiting for the request that actually matches.
func (cb *loopbackCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if !cb.matches(q.Get("state")) {
		http.Error(w, "Unrecognized authorization callback.", http.StatusBadRequest)

		return
	}

	if errParam := q.Get("error"); errParam != "" {
		cb.deliver(callbackResult{err: fmt.Errorf("authorization server returned error: %s", errParam)})
		_, _ = fmt.Fprintln(w, "Authorization failed. You can close this window.")

		return
	}

	// iss is forwarded rather than dropped: RFC 9207 servers (Keycloak,
	// Curity, most OAuth 2.1 ones) advertise
	// authorization_response_iss_parameter_supported, and the SDK hard-fails
	// the exchange when they do and no iss arrives — so dropping it makes
	// login against those servers impossible, and loses the mix-up-attack
	// defense against every other one.
	delivered := cb.deliver(callbackResult{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss")})
	if !delivered {
		http.Error(w, "This authorization was already completed.", http.StatusConflict)

		return
	}

	_, _ = fmt.Fprintln(w, "Authorization complete. You can close this window and return to steps.")
}

// stateFromAuthURL pulls the state parameter out of the authorization URL the
// SDK built. auth.AuthorizationArgs carries only the URL, and state is the
// only thing that distinguishes this attempt's redirect from any other
// request that reaches the loopback port — so it is read back from where the
// SDK put it. An unparsable URL yields "", which matches nothing, leaving the
// flow to time out rather than accept a callback it cannot verify.
func stateFromAuthURL(authURL string) string {
	parsed, err := url.Parse(authURL)
	if err != nil {
		return ""
	}

	return parsed.Query().Get("state")
}

// drain discards a result left in the buffer by a previous attempt, so a
// retry does not read the old attempt's redirect as its own.
func (cb *loopbackCallback) drain() {
	select {
	case <-cb.result:
	default:
	}
}

func (cb *loopbackCallback) Close() {
	_ = cb.server.Close()
}

// fetch returns an auth.AuthorizationCodeFetcher that opens url via open
// and blocks for this listener's callback or ctx cancellation. The URL is
// always printed, not just when open fails: a browser that silently opens
// the wrong profile, opens nothing visible, or is a headless/SSH session's
// no-op looks identical to success from here, and the flow then just hangs
// with nothing on screen to act on. Printing it unconditionally costs one
// line and makes every one of those recoverable by hand.
func (cb *loopbackCallback) fetch(open func(string) error, req authRequest) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		authURL := withScopes(args.URL, req.scopes)

		// Announced before the URL is opened, so handle can already tell this
		// attempt's redirect from a straggler. The SDK generates a fresh
		// state per attempt and may retry Authorize, so a result left over
		// from a previous attempt is drained rather than mistaken for this
		// one's — it would only ever fail the SDK's own state comparison.
		cb.expect(stateFromAuthURL(authURL))
		cb.drain()

		fmt.Printf("\nAuthorize in your browser:\n\n  %s\n\n", authURL)

		// Opened on its own goroutine: cmd.Run waits for the opener to exit,
		// and xdg-open with no registered handler (or a broken DISPLAY over
		// SSH) can block indefinitely — which would make ctx.Done below
		// unreachable and Ctrl-C useless. The URL is printed unconditionally
		// above, so nothing is lost by not waiting.
		go func() {
			err := open(authURL)
			if err != nil {
				fmt.Printf("(could not open a browser automatically: %v — open the URL above)\n", err)
			}
		}()

		select {
		case res := <-cb.result:
			if res.err != nil {
				return nil, res.err
			}

			iss, err := req.resolveIss(res.iss)
			if err != nil {
				return nil, err
			}

			return &auth.AuthorizationResult{Code: res.code, State: res.state, Iss: iss}, nil
		case <-ctx.Done():
			return nil, fmt.Errorf("authorization: %w", ctx.Err())
		}
	}
}
