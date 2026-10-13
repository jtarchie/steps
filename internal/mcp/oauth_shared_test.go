package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// rotatingTokenServer is a token endpoint that rotates the refresh token on every use and refuses a spent one with invalid_grant, as a provider with refresh-token rotation does.
type rotatingTokenServer struct {
	mu        sync.Mutex
	current   string
	refreshes int
}

func newRotatingTokenServer(t *testing.T, initial string) (*httptest.Server, *rotatingTokenServer) {
	t.Helper()

	state := &rotatingTokenServer{current: initial}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		state.mu.Lock()

		if r.PostForm.Get("refresh_token") != state.current {
			state.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})

			return
		}

		state.refreshes++
		n := strconv.Itoa(state.refreshes)
		state.current = "refresh-" + n
		state.mu.Unlock()

		// Holds the answer back so every concurrent caller has read the token file before the rotation is persisted.
		time.Sleep(100 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-" + n, "refresh_token": "refresh-" + n, "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	t.Cleanup(ts.Close)

	return ts, state
}

func (s *rotatingTokenServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.refreshes
}

func saveExpiredLogin(t *testing.T, srv config.MCPServer, tokenURL string) string {
	t.Helper()

	path, err := TokenPath(testPipeline, srv.Name)
	if err != nil {
		t.Fatalf("TokenPath: %v", err)
	}

	tf := &TokenFile{
		Endpoint: srv.Endpoint, ClientID: "client-id", TokenURL: tokenURL,
		AccessToken: "stale", RefreshToken: "refresh-0", Expiry: time.Now().Add(-time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	return path
}

// TestOAuthConcurrentConnectsRefreshOnce: an agent step and a resource poll connecting at once must not both spend a rotating refresh token — the loser's invalid_grant reads as ErrNeedsLogin and forces a human to log in again.
func TestOAuthConcurrentConnectsRefreshOnce(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	useConfigDir(t)

	ts, state := newRotatingTokenServer(t, "refresh-0")
	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}
	saveExpiredLogin(t, srv, ts.URL)

	const connects = 8

	var (
		wg     sync.WaitGroup
		start  = make(chan struct{})
		tokens = make([]string, connects)
		errs   = make([]error, connects)
	)

	for i := range connects {
		wg.Go(func() {
			<-start

			source, err := oauthTokenSource(context.Background(), testPipeline, srv)
			if err != nil {
				errs[i] = err

				return
			}

			tok, err := source.Token()
			if err != nil {
				errs[i] = err

				return
			}

			tokens[i] = tok.AccessToken
		})
	}

	close(start)
	wg.Wait()

	for i := range connects {
		if errs[i] != nil {
			t.Errorf("connect %d: %v", i, errs[i])
		} else if tokens[i] != "access-1" {
			t.Errorf("connect %d got access token %q, want the one refresh's access-1", i, tokens[i])
		}
	}

	if got := state.count(); got != 1 {
		t.Fatalf("token endpoint granted %d refreshes, want exactly 1", got)
	}
}

// TestOAuthTokenSourceAdoptsARefreshSavedElsewhere: another process sharing the login (a `steps run` beside the daemon) may rotate the token first; refreshing with the refresh token this process remembers would spend one that is already gone.
func TestOAuthTokenSourceAdoptsARefreshSavedElsewhere(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	useConfigDir(t)

	ts, state := newRotatingTokenServer(t, "refresh-0")
	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}
	path := saveExpiredLogin(t, srv, ts.URL)

	source, err := oauthTokenSource(context.Background(), testPipeline, srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	state.mu.Lock()
	state.current = "refresh-elsewhere"
	state.mu.Unlock()

	elsewhere := &TokenFile{
		Endpoint: srv.Endpoint, ClientID: "client-id", TokenURL: ts.URL,
		AccessToken: "access-elsewhere", RefreshToken: "refresh-elsewhere", Expiry: time.Now().Add(time.Hour),
	}

	err = elsewhere.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	refreshAndCheck(t, source, "access-elsewhere")

	if got := state.count(); got != 0 {
		t.Fatalf("token endpoint granted %d refreshes, want the saved token used with none", got)
	}
}

// TestOAuthConnectPicksUpALoginDoneMeanwhile: with the token shared for the life of the process, a `steps mcp login` done while a daemon runs must still take effect on the next connect rather than when the old token expires.
func TestOAuthConnectPicksUpALoginDoneMeanwhile(t *testing.T) {
	// Not t.Parallel(): uses t.Setenv.
	useConfigDir(t)

	ts, _ := newRotatingTokenServer(t, "refresh-0")
	srv := config.MCPServer{Name: "linear", Endpoint: "https://mcp.linear.app/mcp", Auth: config.MCPServerAuth{Type: "oauth"}}
	path := saveExpiredLogin(t, srv, ts.URL)

	first, err := oauthTokenSource(context.Background(), testPipeline, srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	refreshAndCheck(t, first, "access-1")

	relogin := &TokenFile{ //nolint:gosec // G101: fixture tokens a fake token endpoint issued
		Endpoint: srv.Endpoint, ClientID: "client-id", TokenURL: ts.URL,
		AccessToken: "access-relogin", RefreshToken: "refresh-relogin", Expiry: time.Now().Add(time.Hour),
	}

	err = relogin.Save(path)
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	second, err := oauthTokenSource(context.Background(), testPipeline, srv)
	if err != nil {
		t.Fatalf("oauthTokenSource: %v", err)
	}

	refreshAndCheck(t, second, "access-relogin")
}
