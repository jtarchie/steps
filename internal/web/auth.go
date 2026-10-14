package web

// The server's own file comment says there is nothing to authenticate against, and that is still true of the loopback default it describes. This file is for the deployment that is not that: a daemon on a public address, where `steps pipeline set` is a remote shell and every page is somebody else's build history. HTTP Basic, against one or more local users — the operator and the CLI are both clients that already carry a URL's userinfo — and each request carries the user it was made by (internal/actor), so what they do is recorded under their name.

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"golang.org/x/crypto/bcrypt"

	"github.com/jtarchie/steps/internal/actor"
)

// authRealm is what a browser's prompt names.
const authRealm = "steps"

// LocalUser is one of the daemon's own accounts: a name and the bcrypt hash of its password, so the daemon never holds the password itself.
type LocalUser struct {
	Name string
	Hash []byte
}

// ErrPlainPassword is a local user given in the clear rather than hashed.
var ErrPlainPassword = errors.New("a local user's password must be a bcrypt hash — make one with `htpasswd -nbB <name> <password>`")

// ParseLocalUser reads one "name:hash" entry, the format htpasswd -B writes. The error never repeats the entry: a plain-text password is exactly what it refuses.
func ParseLocalUser(entry string) (LocalUser, error) {
	name, hash, found := strings.Cut(strings.TrimSpace(entry), ":")
	if !found || name == "" {
		return LocalUser{}, errors.New("a local user is name:hash")
	}

	_, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return LocalUser{}, fmt.Errorf("local user %q: %w", name, ErrPlainPassword)
	}

	return LocalUser{Name: name, Hash: []byte(hash)}, nil
}

// ParseLocalUsers reads an htpasswd file: one entry a line, blank lines and # comments skipped.
func ParseLocalUsers(r io.Reader) ([]LocalUser, error) {
	var users []LocalUser

	scanner := bufio.NewScanner(r)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		user, err := ParseLocalUser(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		users = append(users, user)
	}

	err := scanner.Err()
	if err != nil {
		return nil, fmt.Errorf("read local users: %w", err)
	}

	return users, nil
}

// basicAuth is the credentials this server accepts, never logged and never rendered.
type basicAuth struct {
	// plain is the pair --basic-auth-username/--basic-auth-password gave, kept as given because that is how it arrives.
	plain *plainUser
	users map[string][]byte
	seen  verifiedCache
}

type plainUser struct{ name, password string }

// Option configures a server at construction, which is where the middleware table is built: an auth setting applied afterwards would be a server that answered without it first.
type Option func(*Server)

// WithBasicAuth adds one account given in the clear. Both halves are the caller's to validate — see cli's WebCmd, where half a pair refuses to start.
func WithBasicAuth(username, password string) Option {
	return func(s *Server) { s.authFor().plain = &plainUser{name: username, password: password} }
}

// WithLocalUsers adds hashed accounts; a name given twice is the caller's to refuse.
func WithLocalUsers(users []LocalUser) Option {
	return func(s *Server) {
		auth := s.authFor()
		for _, user := range users {
			auth.users[user.Name] = user.Hash
		}
	}
}

func (s *Server) authFor() *basicAuth {
	if s.auth == nil {
		s.auth = &basicAuth{users: map[string][]byte{}}
	}

	return s.auth
}

// timingHash is compared against when a name is unknown, so a miss costs what a wrong password does and the time taken does not say which names exist.
var timingHash, _ = bcrypt.GenerateFromPassword([]byte("steps-timing"), bcrypt.DefaultCost)

// matches checks a pair. bcrypt is deliberately slow — 50-100ms a check — and the browser and the CLI send credentials on every request, so a success is remembered briefly (verifiedCache).
func (a *basicAuth) matches(username, password string) bool {
	if a.plain != nil && a.plainMatches(username, password) {
		return true
	}

	if a.seen.has(username, password) {
		return true
	}

	hash, known := a.users[username]
	if !known {
		_ = bcrypt.CompareHashAndPassword(timingHash, []byte(password))

		return false
	}

	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil {
		return false
	}

	a.seen.add(username, password)

	return true
}

// ConstantTimeCompare on BOTH halves, and neither short-circuited by the other: `==` on the username leaks which name exists, and `&&` on a correct username leaks the password a byte at a time.
func (a *basicAuth) plainMatches(username, password string) bool {
	user := subtle.ConstantTimeCompare([]byte(username), []byte(a.plain.name))
	pass := subtle.ConstantTimeCompare([]byte(password), []byte(a.plain.password))

	return user&pass == 1
}

// verifiedTTL is how long a verified pair skips bcrypt. Short, because nothing else revokes it: a password changed in the file reaches a running daemon only on restart anyway, so this bounds nothing new.
const verifiedTTL = time.Minute

// verifiedCacheCap bounds the cache; a full one is simply emptied, which costs one bcrypt per active user.
const verifiedCacheCap = 1024

// verifiedCache remembers pairs bcrypt accepted, keyed by a digest of the pair so neither half is held in memory as given.
type verifiedCache struct {
	mu      sync.Mutex
	entries map[[sha256.Size]byte]time.Time
	// now is time.Now outside a test that moves the clock past verifiedTTL.
	now func() time.Time
}

func (c *verifiedCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}

	return time.Now()
}

func verifiedKey(username, password string) [sha256.Size]byte {
	return sha256.Sum256([]byte(username + "\x00" + password))
}

func (c *verifiedCache) has(username, password string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	expires, found := c.entries[verifiedKey(username, password)]

	return found && c.clock().Before(expires)
}

func (c *verifiedCache) add(username, password string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil || len(c.entries) >= verifiedCacheCap {
		c.entries = map[[sha256.Size]byte]time.Time{}
	}

	c.entries[verifiedKey(username, password)] = c.clock().Add(verifiedTTL)
}

// middleware is echo's own, whose 401 already carries the WWW-Authenticate a browser needs in order to prompt.
func (a *basicAuth) middleware() echo.MiddlewareFunc {
	return middleware.BasicAuthWithConfig(middleware.BasicAuthConfig{
		Realm:   authRealm,
		Skipper: skipAuth,
		Validator: func(c *echo.Context, username, password string) (bool, error) {
			if !a.matches(username, password) {
				return false, nil
			}

			c.SetRequest(actingAs(c.Request(), actor.Local(username)))

			return true, nil
		},
	})
}

// actingAs is r carrying who made it, for everything the handler calls.
//
// @contextcheck(req_has_ctx)
func actingAs(r *http.Request, a actor.Actor) *http.Request {
	return r.WithContext(actor.With(r.Context(), a))
}

// MCPCallbackPath is where an authorization server redirects the browser to finish a login this daemon started; the CLI builds the redirect URI from it, so it is one constant.
const MCPCallbackPath = "/mcp/callback"

// HealthPath answers ok to anything that can reach the listener; a platform's health check sends no credentials, and liveness is all it can learn.
const HealthPath = "/healthz"

// Two routes carry their own credential instead of this server's: a webhook sender authenticates with its signature — exempt for exactly the reason it is exempt from the same-origin check and from --read-only — and an oauth redirect with the state its login minted (handleMCPCallback). Neither CAN send a password: one is a machine that was never given it, the other a bare navigation a third party caused. HealthPath is the third exemption, and says nothing.
func skipAuth(c *echo.Context) bool {
	return isHookRoute(c) || c.Path() == MCPCallbackPath || c.Path() == HealthPath
}

// isHookRoute is a webhook delivery, the one route under /p/ whose sender never passed this server's credentials.
func isHookRoute(c *echo.Context) bool {
	return strings.HasSuffix(c.Path(), "/hooks/:resource")
}
