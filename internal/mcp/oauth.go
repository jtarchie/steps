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

// ErrLoginsExist is MoveLogins refusing a destination that already holds
// logins: merging would hand one pipeline another's accounts unnoticed, and
// overwriting would destroy them.
var ErrLoginsExist = errors.New("mcp logins already exist")

// errNoConfigDir is a machine with no user config directory, where no login
// can exist, so moving or removing one has nothing to do.
var errNoConfigDir = errors.New("mcp: resolve user config dir")

// TokenPath returns where pipeline's login for server lives:
// ${XDG_CONFIG_HOME:-~/.config}/steps/mcp/<pipeline>/<server>.json (via
// os.UserConfigDir()), never inside a pipeline's .steps/ directory and never
// in the state database. It is per pipeline because a daemon holds several,
// and a login keyed by server name alone let a login done for one pipeline
// change which account another acted as. Every caller already holds the
// pipeline's config, so the name is passed rather than stamped onto
// config.MCPServer, where it would duplicate cfg.Name and move the cache key.
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

// Save writes t to path atomically (temp file in the same directory, then
// rename), so a concurrent reader — e.g. another `steps web
// --max-concurrent` worker refreshing the same server's token — never
// observes a half-written file. File permissions are 0600 (dir 0700).
// Only a login calls it: it is the one writer allowed to create the
// pipeline's directory.
func (t *TokenFile) Save(path string) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return fmt.Errorf("mkdir %q: %w", dir, err)
	}

	return t.replace(path)
}

// replace is Save without creating the directory, for a refresh write-back:
// after a rename or destroy moved the directory away, recreating it would
// leave a live refresh token under a name a later pipeline inherits.
func (t *TokenFile) replace(path string) error {
	dir := filepath.Dir(path)

	data, err := json.MarshalIndent(t, "", "  ") //nolint:gosec // deliberate: this whole file's job is persisting these secrets to a 0600, non-merkle-hashed, per-pipeline token file — see TokenFile's doc comment
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
func oauthTokenSource(ctx context.Context, pipeline string, srv config.MCPServer) (oauth2.TokenSource, error) {
	tf, path, _, err := checkCredential(pipeline, srv)
	if err != nil {
		return nil, err
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

	err := updated.replace(p.path)
	if err != nil {
		slog.Warn("mcp.oauth.persist_failed", "path", p.path, "error", err,
			"hint", "if the pipeline's login was moved or removed, the rotated token is not saved and it needs a login again")

		return
	}

	p.lastAccess = tok.AccessToken
	p.base = &updated
}
