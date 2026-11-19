package web

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// TestEveryMutatingRouteDeclaresItsRole: a route that changes something says what it asks of the person calling it, in activityRoutes or routeRoles, rather than inheriting whatever the default happens to be.
func TestEveryMutatingRouteDeclaresItsRole(t *testing.T) {
	t.Parallel()

	// Every feature switched on, so every route exists to be held to the tables.
	server, _ := exportServer(t)
	seen := mutatingRoutes(server)

	if len(seen) == 0 {
		t.Fatal("the walk found no mutating routes, so it proves nothing")
	}

	for key := range seen {
		_, recorded := activityRoutes[key]
		_, declared := routeRoles[key]

		if !recorded && !declared {
			t.Errorf("%s changes something and declares no role in activityRoutes or routeRoles", key)
		}

		if recorded && declared {
			t.Errorf("%s declares its role twice", key)
		}
	}

	for key := range routeRoles {
		if !seen[key] && key != "GET /activity" {
			t.Errorf("routeRoles names %s, which is not a mutating route", key)
		}
	}
}

// TestAnUndeclaredRouteFailsClosed: reading is the default only for reads; anything else nobody declared asks for an operator, so a new button cannot ship open to every viewer.
func TestAnUndeclaredRouteFailsClosed(t *testing.T) {
	t.Parallel()

	var asked Role

	e := echo.New()
	e.Any("/p/:pipeline/undeclared", func(c *echo.Context) error {
		asked = neededRole(c)

		return c.NoContent(http.StatusNoContent)
	})

	for _, tc := range []struct {
		method, path string
		want         Role
	}{
		{http.MethodGet, "/p/demo/undeclared", RoleViewer},
		{http.MethodHead, "/p/demo/undeclared", RoleViewer},
		{http.MethodPost, "/p/demo/undeclared", RoleOperator},
		{http.MethodPut, "/p/demo/undeclared", RoleOperator},
		{http.MethodDelete, "/p/demo/undeclared", RoleOperator},
	} {
		asked = 0

		e.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil))

		if asked != tc.want {
			t.Errorf("%s %s asks %v, want %v", tc.method, tc.path, asked, tc.want)
		}
	}
}
