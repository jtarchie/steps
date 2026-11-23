package web

// A wall display: a TV that cannot type a password, holding a key that shows /wall and nothing else. The key is a token of kind display under a read grant, so it lists, and is revoked, beside the person's agents on /connections, and every reader of tokens that asks for an access or a refresh token refuses it by kind.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
)

const (
	displaysPath  = "/connections/displays"
	displayCookie = "steps_display"
	// displayLife is the key's own expiry: none worth having, since revoking it is how a display is turned off.
	displayLife = 100
	// displayCookieAge is as long as browsers keep a cookie (Chrome caps at 400 days); every page the wall serves sets it again, so a screen left on never reaches it.
	displayCookieAge = 400 * 24 * 60 * 60
	displayNameMax   = 100
)

// handleAddDisplay makes a key for one screen and shows its address once: the key itself is never stored, only its digest.
func (s *Server) handleAddDisplay(c *echo.Context) error {
	ctx := c.Request().Context()
	me := actor.From(ctx).String()

	name := strings.TrimSpace(c.FormValue("name"))
	if runes := []rune(name); len(runes) > displayNameMax {
		name = string(runes[:displayNameMax])
	}

	if name == "" {
		name = s.renderer.localeOf(c).text("conn.display.unnamed")
	}

	now := time.Now()
	client := store.OAuthClient{ID: randomToken(), Name: name, CreatedAt: now}

	err := s.sessions.RegisterClient(ctx, client, maxClients)
	if errors.Is(err, store.ErrTooManyClients) {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "this daemon has registered as many clients and displays as it keeps")
	}

	if err != nil {
		return echoError(err)
	}

	key := randomToken()

	err = s.sessions.IssueToken(ctx, store.OAuthToken{TokenHash: tokenDigest(key), Kind: store.TokenDisplay, Grant: store.Grant{Kind: store.GrantRead, ClientID: client.ID, Actor: me}, CreatedAt: now, ExpiresAt: now.AddDate(displayLife, 0, 0)})
	if err != nil {
		return echoError(err)
	}

	slog.InfoContext(ctx, "web.display_added", "actor", me, "client", client.ID, "name", name)

	c.Response().Header().Set("Cache-Control", "no-store")

	return s.renderConnections(c, map[string]any{"NewDisplay": map[string]string{"Name": name, "URL": s.origin(c) + wallPath + "?key=" + key}})
}

// origin is where a browser reaches this daemon: the configured external address, else the one this request came in on.
func (s *Server) origin(c *echo.Context) string {
	if s.external != "" {
		return s.external
	}

	scheme := "http"
	if servedOverTLS(c.Request()) {
		scheme = "https"
	}

	return scheme + "://" + c.Request().Host
}

// displayActor is who a display key reads as: the person who added it, while this daemon still lets them in. Their role is asked by authorize like anybody's.
func (s *Server) displayActor(ctx context.Context, key string) (actor.Actor, bool) {
	if s.sessions == nil || key == "" {
		return actor.Actor{}, false
	}

	token, live, err := s.sessions.FindToken(ctx, tokenDigest(key))
	if err != nil || !live || token.Kind != store.TokenDisplay || token.Grant.Kind != store.GrantRead {
		return actor.Actor{}, false
	}

	who := actor.Parse(token.Grant.Actor)

	return who, s.signsIn(who)
}

// displayCredential is the display a request to /wall carries, by its cookie. Only /wall: the cookie is scoped there by the browser, and asked for nowhere else here, so a copied cookie opens nothing more.
func (s *Server) displayCredential(c *echo.Context) (actor.Actor, bool) {
	if c.Path() != wallPath {
		return actor.Actor{}, false
	}

	cookie, err := c.Request().Cookie(displayCookie)
	if err != nil {
		return actor.Actor{}, false
	}

	who, ok := s.displayActor(c.Request().Context(), cookie.Value)
	if ok {
		setCookie(c, displayCookie, cookie.Value, wallPath, displayCookieAge)
	}

	return who, ok
}

// enterDisplay swaps a display's address for its cookie and sends the browser on to /wall, so the key is not left in the address bar, the history, or a photo of the screen. A key nobody issued is not one, and falls through to sign-in like any stranger.
func (s *Server) enterDisplay(c *echo.Context) (bool, error) {
	key := c.QueryParam("key")
	if c.Path() != wallPath || key == "" {
		return false, nil
	}

	if _, ok := s.displayActor(c.Request().Context(), key); !ok {
		return false, nil
	}

	setCookie(c, displayCookie, key, wallPath, displayCookieAge)

	//nolint:wrapcheck // echo's redirect error is returned verbatim
	return true, c.Redirect(http.StatusSeeOther, wallPath)
}
