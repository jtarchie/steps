package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// ErrNeedsLogin marks an oauth failure that only a human re-running `steps
// mcp login` can clear: no token on disk, one authorized for a different
// endpoint, an expired token with nothing to refresh it with, or a refresh
// the authorization server actively rejected.
//
// It exists to split one question preflight cannot answer from the error
// text alone: would WAITING fix this? Every other way a server fails to
// answer — DNS, a VPN not up yet, a 502 — is a fact about right now, and a
// `steps web` that quits over one is a watcher found dead on Monday for a
// blip that healed in a minute. These four are facts about the credential,
// and no interval grows a refresh token. See config.Problem.Transient, which
// is what both preflight callers set from this.
var ErrNeedsLogin = errors.New("run `steps mcp login`")

// TokenFile is the on-disk shape persisted per oauth-configured server —
// see tokenPath for where. It holds everything a later process needs to
// silently refresh an access token without repeating discovery or dynamic
// client registration: the resolved client credentials and token endpoint
// (captured once, during `steps mcp login` — see login.go) alongside the
// token itself. Endpoint guards against a stale file authorizing the wrong
// server if a name is ever reused for a different endpoint.
//
// Deliberately NOT stored via internal/store (no OpenStore call, no new
// table) and NOT merkle-hashed — the same trust-boundary treatment
// validateAgentEndpoints gives LLM provider credentials, for a second kind
// of secret. See docs/mcp.md's "Trust boundary" section.
type TokenFile struct {
	Endpoint     string    `json:"endpoint"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	TokenURL     string    `json:"token_url"`
	Scopes       []string  `json:"scopes,omitempty"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// expiryDelta mirrors x/oauth2's own defaultExpiryDelta (token.go), the
// margin by which it treats a token as already expired so a request does not
// leave with a credential that dies in flight.
//
// checkRefreshable has to use the SAME margin, because the two answer the
// same question a few microseconds apart and disagreeing leaves a gap where
// this file says "fine" and x/oauth2 says "expired". A token with no refresh
// token and ten seconds left fell in that gap: it passed the check below,
// then failed inside x/oauth2 with a bare "token expired and refresh token is
// not set" — an error that names neither the server nor the fix, and that
// carries no *oauth2.RetrieveError, so classifyRefreshError leaves it
// transient and a watcher retries a permanently dead credential forever.
// Any clock skew at or above the margin has the same shape.
//
// Kept as a local constant rather than read from x/oauth2, which does not
// export it. If they ever diverge the direction that matters is this one
// being the LARGER, which fails early and honestly rather than late and
// silently.
const expiryDelta = 10 * time.Second

// checkRefreshable reports the one broken state a token file can be in that
// is knowable without a single request: the access token has expired and
// there is no refresh token to trade for a new one.
//
// An unexpired token with no refresh token is deliberately fine here. It
// works right now, which is all a `steps run` needs; refusing it would fail
// a run that would have succeeded. Whether a credential can survive
// UNATTENDED is a different question, and it is answered once, at the only
// moment anything can be done about it — see Login, which refuses to report
// success for a token that expires with nothing to renew it.
func (t *TokenFile) checkRefreshable() error {
	if t.RefreshToken != "" || t.Expiry.IsZero() || time.Now().Before(t.Expiry.Add(-expiryDelta)) {
		return nil
	}

	return fmt.Errorf(
		"the access token expired %s ago and the authorization server issued no refresh token — %w",
		time.Since(t.Expiry).Round(time.Second), ErrNeedsLogin)
}

// token builds the *oauth2.Token x/oauth2 needs from the persisted fields.
func (t *TokenFile) token() *oauth2.Token {
	return &oauth2.Token{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		TokenType:    t.TokenType,
		Expiry:       t.Expiry,
	}
}

// TokenPath returns the per-user path an oauth-configured server's token is
// persisted to: ${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<name>.json (via
// os.UserConfigDir()), never inside a pipeline's own .steps/ directory. An
// OAuth token is a per-user-per-service credential, not a per-pipeline
// execution artifact — this is deliberate: it lets `steps mcp login
// <pipeline> <server>` authorize a server once for every pipeline that
// references it by the same name, and it keeps a pipeline-relative token
// path from having to be threaded through RunJob/RunStep/CheckVersions/RunIn
// and merkle's plan-time version resolution, none of which have (or should
// gain) a reason to know about OAuth tokens.
func TokenPath(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("mcp: resolve user config dir: %w", err)
	}

	return filepath.Join(dir, "steps", "mcp", name+".json"), nil
}

// LoadTokenFile reads and parses the token file at path.
func LoadTokenFile(path string) (*TokenFile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is always TokenPath's own output, never attacker-influenced
	if err != nil {
		return nil, fmt.Errorf("read token file: %w", err)
	}

	var tf TokenFile

	err = json.Unmarshal(data, &tf)
	if err != nil {
		return nil, fmt.Errorf("parse token file: %w", err)
	}

	return &tf, nil
}

// Save writes t to path atomically (temp file in the same directory, then
// rename), so a concurrent reader — e.g. another `steps watch
// --max-concurrent` worker refreshing the same server's token — never
// observes a half-written file. File permissions are 0600 (dir 0700).
func (t *TokenFile) Save(path string) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}

	data, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // deliberate: this whole file's job is persisting these secrets to a 0600, non-merkle-hashed, per-user token file — see TokenFile's doc comment
	if err != nil {
		return fmt.Errorf("marshal token file: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp token file: %w", err)
	}

	tmpPath := tmp.Name()

	err = saveTemp(tmp, data, tmpPath)
	if err != nil {
		return err
	}

	err = os.Chmod(tmpPath, 0o600)
	if err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("chmod temp token file: %w", err)
	}

	err = os.Rename(tmpPath, path)
	if err != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("rename temp token file: %w", err)
	}

	return nil
}

// saveTemp writes data to the already-created tmp file and closes it,
// cleaning up on any failure — split out of Save to keep it simple.
func saveTemp(tmp *os.File, data []byte, tmpPath string) error {
	_, writeErr := tmp.Write(data)

	closeErr := tmp.Close()
	if writeErr != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("write temp token file: %w", writeErr)
	}

	if closeErr != nil {
		_ = os.Remove(tmpPath)

		return fmt.Errorf("close temp token file: %w", closeErr)
	}

	return nil
}

// oauthTokenSource builds a self-refreshing oauth2.TokenSource for an
// oauth-configured server at run/watch time, from its persisted per-user
// token file — never interactive (that's login.go's Login, used only by
// `steps mcp login`). A missing token file, or one persisted for a
// different endpoint, surfaces an actionable error naming the login command
// to run.
func oauthTokenSource(ctx context.Context, srv config.MCPServer) (oauth2.TokenSource, error) {
	path, err := TokenPath(srv.Name)
	if err != nil {
		return nil, err
	}

	tf, err := LoadTokenFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp server %q is not authorized (%w <pipeline> %s): %w", srv.Name, ErrNeedsLogin, srv.Name, err)
	}

	if tf.Endpoint != srv.Endpoint {
		return nil, fmt.Errorf("mcp server %q: authorized for a different endpoint (%w <pipeline> %s again)", srv.Name, ErrNeedsLogin, srv.Name)
	}

	// Caught here rather than left to x/oauth2, which answers this exact
	// state with a bare "token expired and refresh token is not set" — true,
	// but it names neither the server nor the fix, and it arrives only after
	// the transport has already been built. The state is knowable from the
	// file alone, so it is answered from the file alone.
	err = tf.checkRefreshable()
	if err != nil {
		return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
	}

	cfg := &oauth2.Config{
		ClientID:     tf.ClientID,
		ClientSecret: tf.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: tf.TokenURL},
		Scopes:       tf.Scopes,
	}

	return &persistingTokenSource{
		inner:      cfg.TokenSource(ctx, tf.token()),
		path:       path,
		base:       tf,
		lastAccess: tf.AccessToken,
	}, nil
}

// persistingTokenSource wraps an x/oauth2 refreshing TokenSource and writes
// a rotated token back to disk. This is not optional: many providers rotate
// the refresh token on every use, and without write-back the *second*
// refresh under a long-running `steps web` would fail with a stale
// refresh token — a silent correctness bug this wrapper exists to prevent.
// A failed write is logged, not returned — the in-memory token is still
// valid for this process; only a future process would be affected, and
// failing the current call over a persistence hiccup would be worse.
type persistingTokenSource struct {
	inner      oauth2.TokenSource
	path       string
	base       *TokenFile // client_id/secret/token_url/endpoint/scopes to preserve across saves
	lastAccess string     // last-persisted access token, to skip redundant writes
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.inner.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh access token: %w", classifyRefreshError(err))
	}

	if tok.AccessToken != p.lastAccess {
		p.persist(tok)
	}

	return tok, nil
}

// classifyRefreshError marks a refresh failure the caller cannot wait out.
// The authorization server ANSWERING and refusing — a revoked, expired, or
// already-rotated-away refresh token — is what no retry improves. A dial
// failure, a timeout, or a gateway that never reached the server is left
// unmarked, and so stays transient: those are the ones a watcher should
// survive rather than exit over.
//
// *oauth2.RetrieveError alone does NOT mean "answered and refused", which is
// the trap this function was originally written into. x/oauth2 builds that
// error before it parses anything and returns it for ANY non-2xx status
// (internal/token.go: `failureStatus := r.StatusCode < 200 || r.StatusCode >
// 299`), so an HTML 502 from a proxy, a 503 with Retry-After, and a
// Cloudflare interstitial all arrive as RetrieveError too. Marking those
// terminal is precisely the watcher-dead-on-Monday failure ErrNeedsLogin's
// doc comment exists to prevent.
//
// So the test is for positive evidence of a refusal: a structured OAuth error
// response per RFC 6749 §5.2 (ErrorCode set — x/oauth2 populates it from
// either a JSON or form-encoded body), or the 400/401 that section says such
// a response is delivered with, for a server that refuses with an empty body.
// A 5xx, a 429, and a 408 are all excluded by construction: they are facts
// about the infrastructure, not the credential.
func classifyRefreshError(err error) error {
	var retrieve *oauth2.RetrieveError

	if !errors.As(err, &retrieve) {
		return err
	}

	refused := retrieve.ErrorCode != ""
	if retrieve.Response != nil {
		switch retrieve.Response.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized:
			refused = true
		}
	}

	if !refused {
		return err
	}

	return fmt.Errorf("%w — %w", err, ErrNeedsLogin)
}

func (p *persistingTokenSource) persist(tok *oauth2.Token) {
	updated := *p.base
	updated.AccessToken = tok.AccessToken
	updated.TokenType = tok.TokenType
	updated.Expiry = tok.Expiry

	if tok.RefreshToken != "" {
		updated.RefreshToken = tok.RefreshToken
	}

	err := updated.Save(p.path)
	if err != nil {
		slog.Warn("mcp.oauth.persist_failed", "path", p.path, "error", err)

		return
	}

	p.lastAccess = tok.AccessToken
	p.base = &updated
}
