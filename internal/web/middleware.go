package web

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// resolvePipeline is the middleware that turns :pipeline into the loaded
// pipeline every handler under /p/ works from.
func (s *Server) resolvePipeline(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		pipeline := s.Lookup(c.Param("pipeline"))
		// A webhook sender never passed this server's credentials, so it is not told why.
		if pipeline == nil && isHookRoute(c) {
			return echo.NewHTTPError(http.StatusNotFound, ErrNoSuchPipeline.Error())
		}

		if pipeline == nil {
			return s.notServed(c.Param("pipeline"))
		}

		c.Set("pipeline", pipeline)

		return next(c)
	}
}

// pipelineOf returns the pipeline resolved for this request.
func pipelineOf(c *echo.Context) *Pipeline {
	pipeline, _ := c.Get("pipeline").(*Pipeline)

	return pipeline
}

// sameOriginMutations rejects a state-changing request whose Origin is not
// the host it was sent to. Safe methods pass untouched, and a request with no
// Origin header at all passes too — that is a curl or a CLI, not a browser
// being aimed at this port by another page.
func sameOriginMutations(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		method := c.Request().Method
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			return next(c)
		}

		// A webhook is authenticated by its signature and sent by a machine that
		// has no reason to share this origin. Exempting it here rather than
		// mounting it outside the group keeps it under /p/<slug>/, which is
		// what says which pipeline it checks.
		if isHookRoute(c) {
			return next(c)
		}

		origin := c.Request().Header.Get("Origin")
		if origin == "" {
			return next(c)
		}

		if origin != "http://"+c.Request().Host && origin != "https://"+c.Request().Host {
			return echo.NewHTTPError(http.StatusForbidden, "cross-origin request refused")
		}

		return next(c)
	}
}

// refuseFraming closes what sameOriginMutations cannot see: a click inside a framed copy of the UI is a same-origin POST carrying the reader's cached credentials. Set before the handler runs, so a stream has them before its first flush. Referrer-Policy keeps a pipeline's and run's names, which are in every path here, out of the Referer sent to a docs link or an MCP authorization server.
func refuseFraming(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		header := c.Response().Header()
		header.Set("Content-Security-Policy", "frame-ancestors 'none'")
		header.Set("X-Frame-Options", "DENY")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "same-origin")

		return next(c)
	}
}

// No page here calls /api, and to the origin check a DNS-rebinding page is same-origin (its own name arrives as both Host and Origin) while a set is a remote shell; not a Host check, because a reverse proxy forwards the client's — refuseOtherHosts is that, implied on a loopback listen and named with --allowed-host elsewhere (hosts.go).
func refuseBrowsers(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		header := c.Request().Header
		site := header.Get("Sec-Fetch-Site")

		// Origin is the half that holds: every browser sends it on a PUT, POST or DELETE, while Sec-Fetch-Site reaches only https and loopback URLs, which a rebinding page is not.
		if header.Get("Origin") != "" || (site != "" && site != "none") {
			return echo.NewHTTPError(http.StatusForbidden, "the /api routes answer the steps CLI, not a browser")
		}

		// ponytail: on a non-loopback listen without --allowed-host a rebinding page's GET carries neither and is answered, showing it nothing the UI's own pages do not, and its UI POSTs pass sameOriginMutations; the allow-list closes both.
		return next(c)
	}
}
