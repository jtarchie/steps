package web

// The server's own file comment says there is nothing to authenticate against, and that is still true of the loopback default it describes. This file is for the deployment that is not that: a daemon on a public address, where `steps pipeline set` is a remote shell and every page is somebody else's build history. HTTP Basic and nothing else — no sessions, no login command, no RBAC — because the operator and the CLI are the only two clients, and a URL's userinfo is a credential both already know how to carry.

import (
	"crypto/subtle"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
)

// authRealm is what a browser's prompt names.
const authRealm = "steps"

// basicAuth is the one credential pair this server accepts, never logged and never rendered.
type basicAuth struct {
	username string
	password string
}

// Option configures a server at construction, which is where the middleware table is built: an auth setting applied afterwards would be a server that answered without it first.
type Option func(*Server)

// WithBasicAuth demands these credentials on every route but the webhook one. Both halves are the caller's to validate — see cli's WebCmd, where half a pair refuses to start.
func WithBasicAuth(username, password string) Option {
	return func(s *Server) { s.auth = &basicAuth{username: username, password: password} }
}

// ConstantTimeCompare on BOTH halves, and neither short-circuited by the other: `==` on the username leaks which name exists, and `&&` on a correct username leaks the password a byte at a time.
func (a *basicAuth) matches(username, password string) bool {
	user := subtle.ConstantTimeCompare([]byte(username), []byte(a.username))
	pass := subtle.ConstantTimeCompare([]byte(password), []byte(a.password))

	return user&pass == 1
}

// middleware is echo's own, whose 401 already carries the WWW-Authenticate a browser needs in order to prompt.
func (a *basicAuth) middleware() echo.MiddlewareFunc {
	return middleware.BasicAuthWithConfig(middleware.BasicAuthConfig{
		Realm:   authRealm,
		Skipper: skipAuth,
		Validator: func(_ *echo.Context, username, password string) (bool, error) {
			return a.matches(username, password), nil
		},
	})
}

// A webhook sender has no credentials to give and authenticates with its own signature, so the delivery route is exempt for exactly the reason it is exempt from the same-origin check and from --read-only.
func skipAuth(c *echo.Context) bool {
	return strings.HasSuffix(c.Path(), "/hooks/:resource")
}
