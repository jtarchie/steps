package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

func TestARoleRuleMustCompileAndNameARole(t *testing.T) {
	t.Parallel()

	for spec, want := range map[string]string{
		"boss=true":                    "viewer, operator, admin",
		"operator":                     "role=rule",
		`admin=UserID ==`:              "--role admin",
		`admin=Nonsense == "x"`:        "--role admin",
		"operator=true":                "admin rule",
		`admin=UserID == "a"`:          "",
		`admin=UserID == "a" || false`: "",
	} {
		_, err := CompileRoles([]string{spec})

		switch {
		case want == "" && err != nil:
			t.Errorf("CompileRoles(%q) = %v, want accepted", spec, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("CompileRoles(%q) = %v, want refused naming %q", spec, err, want)
		}
	}

	_, err := CompileRoles([]string{"admin=true", "admin=false"})
	if err == nil {
		t.Error("a role given twice was accepted")
	}
}

func TestEachRoleIncludesTheOnesBelow(t *testing.T) {
	t.Parallel()

	roles, err := CompileRoles([]string{`admin=UserID == "root" || ID == "583231"`, `operator="acme/eng" in Groups`, `viewer=Email endsWith "@acme.dev"`})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		id   Identity
		want Role
	}{
		{Identity{UserID: "root"}, RoleAdmin},
		{Identity{UserID: "dan", Groups: []string{"acme/eng"}}, RoleOperator},
		{Identity{UserID: "eve", Email: "eve@acme.dev"}, RoleViewer},
		{Identity{UserID: "mallory", Email: "m@evil.example"}, 0},
		{Identity{UserID: "renamed", ID: "583231"}, RoleAdmin},
	} {
		if got := roles.highest(tc.id); got != tc.want {
			t.Errorf("%+v is %v, want %v", tc.id, got, tc.want)
		}
	}
}

// TestAnExclusionRuleReadsTheStoredGroups: a rule written as "not in this group" is only as good as the groups it is evaluated over. They come from the daemon's own record of the person, never from anything the browser carries, so a contractor cannot pass by arriving with nothing to say.
func TestAnExclusionRuleReadsTheStoredGroups(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	roles, err := CompileRoles([]string{`admin=false`, `operator=!("acme/contractors" in Groups)`, `viewer=Provider == "github"`})
	if err != nil {
		t.Fatal(err)
	}

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithSignInProviders(GitHubSignIn("c", "s", "http://x", nil)), WithRoles(roles))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for who, groups := range map[string][]string{"carol": {"acme/contractors"}, "dan": {"acme/eng"}} {
		err := accounts.SaveUser(t.Context(), store.User{Actor: "github:" + who, Groups: groups, RefreshedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}

		err = accounts.CreateSession(t.Context(), store.Session{TokenHash: tokenDigest(who + "-token"), Actor: "github:" + who, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}

	if body := postAs(t, server, "carol-token"); !strings.Contains(body, "needs the operator role") {
		t.Errorf("a contractor reached the trigger handler: %s", body)
	}

	if body := postAs(t, server, "dan-token"); strings.Contains(body, "needs the operator role") {
		t.Errorf("an engineer was refused the operator role: %s", body)
	}
}

func postAs(t *testing.T, server *Server, token string) string {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/p/demo/jobs/build/trigger", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Body.String()
}

// TestARuleCanNameTheAccountRatherThanTheLogin: the stored account id reaches the rules, so a rule written against GitHub's numeric id holds for that account whatever its login is now, and not for a newcomer who registers the old login.
func TestARuleCanNameTheAccountRatherThanTheLogin(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithSignInProviders(GitHubSignIn("c", "s", "http://x", nil)), WithRoles(githubRoles(t)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for login, accountID := range map[string]string{"alice-renamed": "583231", "alice": "999999"} {
		err := accounts.SaveUser(t.Context(), store.User{Actor: "github:" + login, AccountID: accountID, RefreshedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}

	if id := server.identityOf(t.Context(), actor.Actor{Kind: "github", ID: "alice-renamed"}); id.ID != "583231" || id.UserID != "alice-renamed" {
		t.Errorf("identityOf(alice-renamed) = %+v; want the account id and the current login", id)
	}

	if id := server.identityOf(t.Context(), actor.Actor{Kind: "github", ID: "alice"}); id.ID != "999999" {
		t.Errorf("identityOf(alice) = %+v; the newcomer must carry their own account id, not the old one", id)
	}
}

// githubRoles are the rules a test daemon offering GitHub sign-in needs before it will start: anybody from GitHub may read, nobody administers.
func githubRoles(t *testing.T) Roles {
	t.Helper()

	roles, err := CompileRoles([]string{`admin=false`, `viewer=Provider == "github"`})
	if err != nil {
		t.Fatal(err)
	}

	return roles
}

// TestAServerWithAProviderNeedsRules: GitHub and Google let in anybody with an account there, so the server refuses to be built with no rules about them — the zero rules make everyone signed in an admin — or with no viewer rule, which would let all of them read.
func TestAServerWithAProviderNeedsRules(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	github := WithSignInProviders(GitHubSignIn("c", "s", "http://x", nil))

	adminOnly, err := CompileRoles([]string{`admin=UserID == "alex"`})
	if err != nil {
		t.Fatal(err)
	}

	for name, opts := range map[string][]Option{
		"no rules":       {github},
		"no viewer rule": {github, WithRoles(adminOnly)},
	} {
		_, err := New([]*Pipeline{pipeline}, nil, opts...)
		if err == nil {
			t.Errorf("%s: a server offering GitHub sign-in was built", name)
		}
	}

	_, err = New([]*Pipeline{pipeline}, nil, github, WithRoles(githubRoles(t)))
	if err != nil {
		t.Errorf("with admin and viewer rules: %v", err)
	}

	_, err = New([]*Pipeline{pipeline}, nil, WithLocalUsers([]LocalUser{{Name: "alex", Hash: []byte(mustHash(t, "a"))}}))
	if err != nil {
		t.Errorf("local accounts alone need no rules, as before: %v", err)
	}
}
