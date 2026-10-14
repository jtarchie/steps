package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/labstack/echo/v5"
)

// WithAllowedHosts refuses every request whose Host is not one of these names or a loopback one. Opt-in because a reverse proxy forwards the client's Host, so only the operator knows which names are this daemon's.
func WithAllowedHosts(hosts []string) Option {
	return func(s *Server) {
		if len(hosts) == 0 {
			return
		}

		s.allowedHosts = map[string]bool{}
		for _, host := range hosts {
			s.allowedHosts[hostName(host)] = true
		}
	}
}

// refuseOtherHosts closes DNS rebinding, which the origin check cannot see: a rebinding page's own name arrives as both Host and Origin. Uniform, webhooks included — a provider posts to the name the operator gave it, which belongs in the list. HealthPath is exempt because a platform's probe names the pod's address and learns only "ok".
func (s *Server) refuseOtherHosts(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Path() == HealthPath {
			return next(c)
		}

		host := hostName(c.Request().Host)
		if s.allowedHosts[host] || isLoopbackName(host) {
			return next(c)
		}

		return echo.NewHTTPError(http.StatusForbidden, "this daemon does not answer to that host name — see --allowed-host")
	}
}

// hostName is a Host header reduced to the name a list compares: port, IPv6 brackets and a trailing dot dropped, case folded.
func hostName(host string) string {
	name, _, err := net.SplitHostPort(host)
	if err == nil {
		host = name
	}

	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// isLoopbackName is always allowed, so the CLI on this machine keeps working; a rebinding page cannot present it, since its name is the one it registered.
func isLoopbackName(host string) bool {
	if host == "localhost" {
		return true
	}

	addr, err := netip.ParseAddr(host)

	return err == nil && addr.IsLoopback()
}
