package web

// `steps mcp login -p <pipeline> --target <daemon>`: an oauth login the DAEMON runs, because the daemon is the machine that spends the token and a machine with no browser cannot be the loopback a redirect comes back to. Three routes: two under /api for the CLI, and the one a provider sends the browser to.

import (
	"context"
	"net/http"

	"github.com/labstack/echo/v5"
)

// Login states, which the CLI polls for.
const (
	LoginPending    = "pending"
	LoginAuthorized = "authorized"
	LoginFailed     = "failed"
)

// LoginStatus is where one login stands. Shared with the client for the reason SetRequest is.
type LoginStatus struct {
	State string `json:"state"`
	// AuthorizeURL appears once discovery and registration are done, which is network work the start request does not wait out.
	AuthorizeURL string `json:"authorize_url,omitempty"`
	// Message is the refusal, in the flow's own words — including the one that matters most unattended: authorized, but with a token that cannot be renewed.
	Message   string `json:"message,omitempty"`
	TokenPath string `json:"token_path,omitempty"`
}

// LoginRequest starts one. Base is the address the CLI reached this daemon on, userinfo already removed: it is proven to work, the browser finishing the flow sits beside that CLI, and it is what the redirect URI is built from — so a password left on it would be handed to the authorization server.
type LoginRequest struct {
	Base string `json:"base"`
}

// Authorizer runs logins. An interface for the reason Manager is one — depguard keeps internal/mcp out of this package — and optional: a manager that is not one answers 501 rather than pretending.
type Authorizer interface {
	StartLogin(pipeline *Pipeline, server string, req LoginRequest) (LoginStatus, error)
	LoginStatus(server string) (LoginStatus, bool)
	// LoginCallback is the handler for the pending login that minted state, nil when none did.
	LoginCallback(state string) http.Handler
}

func (s *Server) authorizer() Authorizer {
	authorizer, _ := s.held().(Authorizer)

	return authorizer
}

func (s *Server) handleAPIStartLogin(c *echo.Context) error {
	target := s.Lookup(c.Param("pipeline"))
	if target == nil {
		return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
	}

	authorizer := s.authorizer()
	if authorizer == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "this daemon cannot run an mcp login")
	}

	var req LoginRequest

	err := c.Bind(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "could not read the login request")
	}

	status, err := authorizer.StartLogin(target, c.Param("server"), req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	//nolint:wrapcheck // echo writes the response; an encoding failure is reported verbatim
	return c.JSON(http.StatusAccepted, status)
}

func (s *Server) handleAPILoginStatus(c *echo.Context) error {
	authorizer := s.authorizer()
	if authorizer == nil {
		return echo.NewHTTPError(http.StatusNotImplemented, "this daemon cannot run an mcp login")
	}

	status, found := authorizer.LoginStatus(c.Param("server"))
	if !found {
		return echo.NewHTTPError(http.StatusNotFound, "no login has been started for this server")
	}

	//nolint:wrapcheck // echo writes the response; an encoding failure is reported verbatim
	return c.JSON(http.StatusOK, status)
}

// handleMCPCallback is where an authorization server sends the browser. It asks for no credentials (see skipAuth) because the request cannot carry any: it is authenticated by its state, a nonce minted for one login and seen only by whoever was handed that login's authorization URL. A state nobody is waiting on is a 404 — not a 401, which would prompt, and not a 400, which would confirm the route is listening for something.
func (s *Server) handleMCPCallback(c *echo.Context) error {
	var handler http.Handler

	if authorizer := s.authorizer(); authorizer != nil {
		handler = authorizer.LoginCallback(c.QueryParam("state"))
	}

	if handler == nil {
		return echo.NewHTTPError(http.StatusNotFound, "no login is waiting for this authorization")
	}

	handler.ServeHTTP(c.Response(), c.Request().WithContext(context.WithoutCancel(c.Request().Context())))

	return nil
}
