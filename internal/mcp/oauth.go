package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// TokenFile is one pipeline's login for one oauth-configured server, as handed to the Vault. It holds everything a later process needs to
// silently refresh an access token without repeating discovery or dynamic
// client registration: the resolved client credentials and token endpoint
// (captured once, during `steps mcp login` — see login.go) alongside the
// token itself. Endpoint guards against a stale login authorizing the wrong
// server if a name is ever reused for a different endpoint.
//
// Never merkle-hashed, and never reaches the state database unsealed: the vault internal/cli installs encrypts it first. See docs/mcp.md's "Trust boundary" section.
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
	// scope is which pipeline the vault read this under (Vault.Load), so a write-back lands on that pipeline or nowhere — never on a stranger that took its name since. Empty for a login not yet saved.
	scope string
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

// checkRefreshable reports the one broken state a login can be in that
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

// ErrLoginsExist is MoveLogins refusing a destination that already holds
// logins: merging would hand one pipeline another's accounts unnoticed, and
// overwriting would destroy them.
var ErrLoginsExist = errors.New("mcp logins already exist")

// errNoConfigDir is a machine with no user config directory, where no login
// can exist, so moving or removing one has nothing to do.
var errNoConfigDir = errors.New("mcp: resolve user config dir")

// TokenPath is where steps before the state database kept pipeline's login for server, in the clear:
// ${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<pipeline>/<server>.json (via
// os.UserConfigDir()). A login still there is imported on first use (importLegacyLogin) and the file removed.
func TokenPath(pipeline, server string) (string, error) {
	if server == "" {
		return "", errors.New("mcp: token path needs a server name")
	}

	dir, err := loginsDir(pipeline)
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, segment(server)+".json"), nil
}

// loginsDir is the one directory holding pipeline's logins. An empty name is
// refused because it would resolve to the parent of every pipeline's logins,
// which ForgetLogins would then remove whole.
func loginsDir(pipeline string) (string, error) {
	if pipeline == "" {
		return "", errors.New("mcp: token path needs a pipeline name")
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoConfigDir, err)
	}

	return filepath.Join(dir, "steps", "mcp", segment(pipeline)), nil
}

var verbatimSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// segment makes name one path segment that cannot traverse and cannot fold
// into another name's segment on a case-insensitive filesystem (APFS's
// default): `App` and `app` sharing one directory is the cross-pipeline
// sharing TokenPath exists to remove. A name already in the safe alphabet is
// kept as written so the directory stays recognizable; anything else is
// lowercased, sanitized and suffixed with `~` and a hash of the original,
// and `~` never appears in the verbatim form, so the two cannot collide.
// A name ending `.json` is hashed too: kept verbatim it would be the path of
// a pre-pipeline login file (mcp/<server>.json), a file where a directory
// is expected.
func segment(name string) string {
	if len(name) <= 200 && verbatimSegment.MatchString(name) && !strings.HasSuffix(name, ".json") {
		return name
	}

	safe := []byte(strings.Map(safeRune, strings.ToLower(name)))
	if len(safe) > 64 {
		safe = safe[:64]
	}

	sum := sha256.Sum256([]byte(name))

	return string(safe) + "~" + hex.EncodeToString(sum[:])[:12]
}

func safeRune(r rune) rune {
	if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
		return r
	}

	return '-'
}

// LoginsExist reports whether pipeline already has a login directory, and
// where, so a rename can refuse before anything has moved. A directory that
// cannot be looked at counts as existing: the rename it guards would
// otherwise go ahead over whatever is there.
func LoginsExist(pipeline string) (string, bool) {
	dir, err := loginsDir(pipeline)
	if err != nil {
		return "", false
	}

	_, err = os.Lstat(dir)

	return dir, !errors.Is(err, fs.ErrNotExist)
}

// MoveLogins carries from's logins to to, for a pipeline rename, which keeps
// history and so keeps its logins. A pipeline with no logins is a no-op; a
// destination that exists, even empty, is ErrLoginsExist, checked here
// because os.Rename silently replaces an empty directory on Linux.
func MoveLogins(from, to string) error {
	src, err := loginsDir(from)
	if err != nil {
		return ignoreNoConfigDir(err)
	}

	dst, err := loginsDir(to)
	if err != nil {
		return ignoreNoConfigDir(err)
	}

	_, err = os.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("mcp: move logins: %w", err)
	}

	_, err = os.Lstat(dst)
	if err == nil {
		return fmt.Errorf("%w at %s", ErrLoginsExist, dst)
	}

	if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("mcp: move logins: %w", err)
	}

	err = os.Rename(src, dst)
	if err != nil {
		return fmt.Errorf("mcp: move logins: %w", err)
	}

	return nil
}

func ignoreNoConfigDir(err error) error {
	if errors.Is(err, errNoConfigDir) {
		return nil
	}

	return err
}

// ForgetLogins removes every login pipeline has, for a destroy, so a later
// pipeline given the same name does not silently inherit them.
func ForgetLogins(pipeline string) error {
	dir, err := loginsDir(pipeline)
	if err != nil {
		return ignoreNoConfigDir(err)
	}

	err = os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("mcp: remove logins: %w", err)
	}

	return nil
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

// oauthTokenSource builds a self-refreshing oauth2.TokenSource for an
// oauth-configured server at run/watch time, from the pipeline's
// login in the vault — never interactive (that's login.go's Login, used only by
// `steps mcp login`). A missing login, or one persisted for a
// different endpoint, surfaces an actionable error naming the login command
// to run.
//
// Every connect for one login shares one sharedToken, because a connect is
// per call (an agent's tools, a resource's poll) and two of them refreshing
// independently would each spend the same rotating refresh token — the
// second gets invalid_grant, which classifyRefreshError rightly reads as
// ErrNeedsLogin, and a working credential demands a human.
func oauthTokenSource(ctx context.Context, pipeline string, srv config.MCPServer) (oauth2.TokenSource, error) {
	tf, _, err := checkCredential(ctx, pipeline, srv)
	if err != nil {
		return nil, err
	}

	return &connTokenSource{ctx: ctx, shared: sharedTokenFor(ctx, pipeline, srv.Name, tf)}, nil
}

var sharedTokens = struct {
	mu    sync.Mutex
	byKey map[string]*sharedToken
}{byKey: map[string]*sharedToken{}}

// sharedTokenFor returns the process's one sharedToken for pipeline's login to server. An existing one re-reads the vault, so a `steps mcp login` done while a daemon runs takes effect on the next connect.
func sharedTokenFor(ctx context.Context, pipeline, server string, tf *TokenFile) *sharedToken {
	sharedTokens.mu.Lock()
	defer sharedTokens.mu.Unlock()

	key := pipeline + "\x00" + server

	shared, ok := sharedTokens.byKey[key]
	if !ok {
		shared = &sharedToken{pipeline: pipeline, server: server, file: tf, saved: tf.AccessToken}
		sharedTokens.byKey[key] = shared

		return shared
	}

	shared.mu.Lock()
	shared.reload(ctx)
	shared.mu.Unlock()

	return shared
}

// connTokenSource is one connect's handle on a sharedToken, carrying the
// context a refresh it triggers is made under.
type connTokenSource struct {
	ctx    context.Context //nolint:containedctx // oauth2.TokenSource.Token takes no context; x/oauth2's own sources hold one the same way
	shared *sharedToken
}

func (c *connTokenSource) Token() (*oauth2.Token, error) {
	return c.shared.token(c.ctx)
}

// sharedToken is a login's token, refreshed by one caller at a time and
// written back to the vault. The write-back is not optional: many providers
// rotate the refresh token on every use, so the next process to start must
// see the new one. A failed write is logged, not returned — the in-memory
// token is still valid for this process, and failing the current call over
// a persistence hiccup would be worse.
//
// The refresh happens under mu, which is the single flight: a caller that
// waited finds the token the first one fetched and returns it.
type sharedToken struct {
	mu       sync.Mutex
	pipeline string
	server   string
	file     *TokenFile
	saved    string // the access token the vault holds as of this process's last read or write of it
}

func (s *sharedToken) token(ctx context.Context) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if tok := s.file.token(); tok.Valid() {
		return tok, nil
	}

	s.reload(ctx)

	if tok := s.file.token(); tok.Valid() {
		return tok, nil
	}

	cfg := &oauth2.Config{
		ClientID:     s.file.ClientID,
		ClientSecret: s.file.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: s.file.TokenURL},
		Scopes:       s.file.Scopes,
	}

	tok, err := cfg.TokenSource(ctx, s.file.token()).Token()
	if err != nil {
		return nil, fmt.Errorf("refresh access token: %w", classifyRefreshError(err))
	}

	s.persist(ctx, tok)

	return tok, nil
}

// reload adopts the saved login when something other than this sharedToken has written
// it — a fresh login, or a refresh by a process sharing the database. A login still holding what this process last saw is
// ignored: after a failed write it holds a refresh token already spent.
func (s *sharedToken) reload(ctx context.Context) {
	saved, err := loadLogin(ctx, s.pipeline, s.server)
	if err != nil || saved == nil || saved.AccessToken == s.saved {
		return
	}

	s.file = saved
	s.saved = saved.AccessToken
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

// persistBound caps the write-back, which runs under its own context: a refresh's caller going away right after is exactly when a rotated refresh token must still land.
const persistBound = 10 * time.Second

func (s *sharedToken) persist(ctx context.Context, tok *oauth2.Token) {
	updated := *s.file
	updated.AccessToken = tok.AccessToken
	updated.TokenType = tok.TokenType
	updated.Expiry = tok.Expiry

	if tok.RefreshToken != "" {
		updated.RefreshToken = tok.RefreshToken
	}

	s.file = &updated

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistBound)
	defer cancel()

	err := saveLogin(writeCtx, s.pipeline, s.server, &updated)
	if err != nil {
		slog.WarnContext(ctx, "mcp.oauth.persist_failed", "pipeline", s.pipeline, "server", s.server, "error", err,
			"hint", "the rotated token is not saved, so a later process needs a login again")

		return
	}

	s.saved = tok.AccessToken
}
