package web

// Signing in from a browser: a form instead of the Basic prompt, and a session
// the daemon keeps (store.Accounts) instead of a password the browser resends
// on every request — which is what makes signing out possible at all.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
)

const (
	loginPath     = "/login"
	logoutPath    = "/logout"
	sessionCookie = "steps_session"
	// sessionTTL is how long a sign-in lasts. A week, because the daemon is a tool people keep a tab open on; signing out, or the operator removing the account and restarting, ends it sooner.
	sessionTTL = 7 * 24 * time.Hour
)

// WithSessions lets browsers sign in through a form, keeping their sessions in accounts.
func WithSessions(accounts store.Accounts) Option {
	return func(s *Server) { s.sessions = accounts }
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))

	return hex.EncodeToString(sum[:])
}

// sessionActor is who the request's session cookie signs in as, if it names a live session.
func (s *Server) sessionActor(c *echo.Context) (actor.Actor, bool) {
	if s.sessions == nil {
		return actor.Actor{}, false
	}

	cookie, err := c.Request().Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return actor.Actor{}, false
	}

	session, found, err := s.sessions.FindSession(c.Request().Context(), tokenDigest(cookie.Value))
	if err != nil {
		slog.ErrorContext(c.Request().Context(), "web.session_unreadable", "error", err)

		return actor.Actor{}, false
	}

	who := actor.Parse(session.Actor)
	if !found || !s.signsIn(who) {
		return actor.Actor{}, false
	}

	return who, true
}

// signsIn is whether who is still somebody this daemon lets in: a local account it still holds, or a provider it still offers. A session or a grant outlives the flag that admitted it otherwise, and removing the account and restarting would end nothing.
func (s *Server) signsIn(who actor.Actor) bool {
	if who.Kind == "local" {
		return s.auth.knows(who.ID)
	}

	_, offered := s.providerNamed(who.Kind)

	return offered
}

// safeNext is where a sign-in may send the browser on: a path on this server and nothing else. "//" and "/\\" are hosts to a browser, which is how a sign-in page becomes a way off the site — and a browser strips tabs and newlines from a URL before reading it, so "/\t/evil" is "//evil" by the time it is followed.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\t\r\n") {
		return next
	}

	return "/"
}

func (s *Server) handleLoginPage(c *echo.Context) error {
	return s.renderLogin(c, http.StatusOK, "", "")
}

// renderLogin draws the form with an empty nav: whoever is reading has not signed in, so the pipeline names the header usually lists are not theirs to see.
func (s *Server) renderLogin(c *echo.Context, status int, username, problem string) error {
	//nolint:wrapcheck // render errors surface through the shared error handler
	return c.Render(status, "login", map[string]any{
		"Nav":       navData{URL: c.Request().URL.RequestURI()},
		"Title":     "sign in",
		"Next":      safeNext(c.QueryParam("next")),
		"Username":  username,
		"Problem":   problem,
		"Providers": s.providers,
		"Local":     s.auth.hasAccounts(),
	})
}

func (s *Server) handleLogin(c *echo.Context) error {
	username, password := c.FormValue("username"), c.FormValue("password")

	// One sentence for every failure: which half was wrong is what somebody guessing names wants to know.
	if !s.auth.matches(username, password) {
		return s.renderLogin(c, http.StatusUnauthorized, username, "couldn't sign you in")
	}

	err := s.startSession(c, actor.Local(username))
	if err != nil {
		return echoError(err)
	}

	slog.InfoContext(c.Request().Context(), "web.signed_in", "actor", actor.Local(username).String())

	//nolint:wrapcheck // echo's redirect error is returned verbatim by every handler here
	return c.Redirect(http.StatusSeeOther, safeNext(c.FormValue("next")))
}

// startSession signs the browser in as who, whichever way they proved it.
func (s *Server) startSession(c *echo.Context, who actor.Actor) error {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()

	err := s.sessions.CreateSession(c.Request().Context(), store.Session{
		TokenHash: tokenDigest(token),
		Actor:     who.String(),
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
	})
	if err != nil {
		return err //nolint:wrapcheck // the store names what it was recording
	}

	// Secure exactly when the browser came over https: a daemon on plain-HTTP loopback, the default, would have a Secure cookie dropped and never sign anybody in.
	c.SetCookie(&http.Cookie{ //nolint:gosec // Secure is servedOverTLS, see above
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   servedOverTLS(c.Request()),
	})

	return nil
}

// handleLogout ends the session on the daemon, not only in this browser: a copied cookie stops working too.
func (s *Server) handleLogout(c *echo.Context) error {
	cookie, err := c.Request().Cookie(sessionCookie)
	if err == nil && cookie.Value != "" {
		err = s.sessions.RevokeSession(context.WithoutCancel(c.Request().Context()), tokenDigest(cookie.Value))
		if err != nil {
			return echoError(err)
		}
	}

	slog.InfoContext(c.Request().Context(), "web.signed_out", "actor", actor.From(c.Request().Context()).String())

	c.SetCookie(&http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: servedOverTLS(c.Request())}) //nolint:gosec // as handleLogin

	//nolint:wrapcheck // as above
	return c.Redirect(http.StatusSeeOther, loginPath)
}

// servedOverTLS is whether the browser reached this daemon over https, directly or through a proxy that terminated it — the one case a cookie can be marked Secure without being dropped.
func servedOverTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionKey marks a request authenticate let in by its session, so the page can offer to end it without reading the session twice.
const sessionKey = "steps.session"

// hasSession reports whether the request signed in through the form, which is what can be signed out of.
func (s *Server) hasSession(c *echo.Context) bool {
	signedIn, _ := c.Get(sessionKey).(bool)

	return signedIn
}
