package web

// Who may do what. Concourse's vocabulary — viewer, operator, admin — with
// each role granted by an expression about the signed-in person rather than a
// list of names, so "everyone at acme" and "the oncall team" are one line
// each. The daemon's rules are the only ones; nothing a pipeline says can
// grant a role here.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/labstack/echo/v5"

	"github.com/jtarchie/steps/internal/actor"
)

// Role is what a person may do. Each includes the ones below it.
type Role int

const (
	// RoleViewer reads every page.
	RoleViewer Role = iota + 1
	// RoleOperator acts on runs: trigger, retry, approve, answer, abort, release, pause.
	RoleOperator
	// RoleAdmin changes what the daemon serves: set, destroy, rename, connect an MCP server, and reads the activity log.
	RoleAdmin
)

var roleNames = map[string]Role{"viewer": RoleViewer, "operator": RoleOperator, "admin": RoleAdmin}

func (r Role) String() string {
	for name, role := range roleNames {
		if role == r {
			return name
		}
	}

	return "none"
}

// Identity is what a role's rule is written against.
type Identity struct {
	// Provider is where the name came from: local, github, google.
	Provider string
	// UserID is the name within it: a local account, a GitHub login, a Google address.
	UserID        string
	Email         string
	Organizations []string
	Groups        []string
}

// Roles are compiled rules; the zero value grants everything to anyone signed in, which is a daemon that has not been told otherwise.
type Roles struct {
	rules map[Role]*vm.Program
}

// CompileRoles reads "role=expression" rules. Compiled here, once, so a rule that cannot work stops the daemon before it serves anyone. Rules are expected about who is allowed, so an admin rule is required: without one nobody could set a pipeline, and the daemon would be stuck as it stands.
func CompileRoles(specs []string) (Roles, error) {
	if len(specs) == 0 {
		return Roles{}, nil
	}

	roles := Roles{rules: map[Role]*vm.Program{}}

	for _, spec := range specs {
		name, rule, found := strings.Cut(spec, "=")
		role, known := roleNames[strings.TrimSpace(name)]

		if !found || !known {
			return Roles{}, fmt.Errorf("--role %q is role=rule, with role one of viewer, operator, admin", spec)
		}

		if _, twice := roles.rules[role]; twice {
			return Roles{}, fmt.Errorf("--role %s is given twice", name)
		}

		program, err := expr.Compile(rule, expr.Env(Identity{}), expr.AsBool())
		if err != nil {
			return Roles{}, fmt.Errorf("--role %s: %w", name, err)
		}

		roles.rules[role] = program
	}

	if roles.rules[RoleAdmin] == nil {
		return Roles{}, errors.New("--role needs an admin rule as well: without one nobody could set, rename or destroy a pipeline")
	}

	return roles, nil
}

// WithRoles limits who may do what to these rules.
func WithRoles(roles Roles) Option {
	return func(s *Server) { s.roles = roles }
}

// highest is the most a person may do: the highest role whose rule holds, each role including the ones below. A viewer rule left out admits anyone signed in to read.
func (r Roles) highest(id Identity) Role {
	if r.rules == nil {
		return RoleAdmin
	}

	for _, role := range []Role{RoleAdmin, RoleOperator, RoleViewer} {
		program, written := r.rules[role]
		if !written {
			if role == RoleViewer {
				return RoleViewer
			}

			continue
		}

		granted, err := expr.Run(program, id)
		if err != nil {
			continue
		}

		if allowed, _ := granted.(bool); allowed {
			return role
		}
	}

	return 0
}

// identityOf is what the rules see of a person: for a provider's account, what it said at their last sign-in, read from the daemon's own record and never from anything the browser carries.
func (s *Server) identityOf(ctx context.Context, who actor.Actor) Identity {
	id := Identity{Provider: who.Kind, UserID: who.ID}

	if s.sessions == nil || who.Kind == "local" {
		return id
	}

	user, found, err := s.sessions.FindUser(ctx, who.String())
	if err != nil {
		slog.ErrorContext(ctx, "web.user_unreadable", "actor", who.String(), "error", err)

		return id
	}

	if found {
		id.Email, id.Organizations, id.Groups = user.Email, user.Organizations, user.Groups
	}

	return id
}

const roleKey = "steps.role"

// roleOf is what this request's person may do; everything on a server nobody signs in to.
func (s *Server) roleOf(c *echo.Context) Role {
	if role, ok := c.Get(roleKey).(Role); ok {
		return role
	}

	return RoleAdmin
}

// neededRole is what a route asks of the person calling it.
func neededRole(c *echo.Context) Role {
	if route, recorded := activityRoutes[c.Request().Method+" "+c.Path()]; recorded {
		return route.role
	}

	if c.Path() == "/activity" {
		return RoleAdmin
	}

	return RoleViewer
}

// authorize refuses a request its person's role does not cover, on the server, whatever the page drew: a hidden button is a courtesy and this is the check.
func (s *Server) authorize(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if s.skipsAuth(c) || c.Path() == logoutPath {
			return next(c)
		}

		who := actor.From(c.Request().Context())
		role := s.roles.highest(s.identityOf(c.Request().Context(), who))
		c.Set(roleKey, role)

		needed := neededRole(c)
		if role >= needed {
			return next(c)
		}

		if role == 0 {
			return echo.NewHTTPError(http.StatusForbidden, who.Label()+" is signed in but has no role on this daemon")
		}

		return echo.NewHTTPError(http.StatusForbidden, "this needs the "+needed.String()+" role, and "+who.Label()+" is "+aOrAn(role.String()))
	}
}

func aOrAn(word string) string {
	if slices.Contains([]byte("aeiou"), word[0]) {
		return "an " + word
	}

	return "a " + word
}
