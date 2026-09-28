package resource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// TestListToolsDoesNotCacheAServerThatNeedsALogin is internal/agent's
// TestPreflightDoesNotCacheAServerThatNeedsALogin, for the cache on this side
// of the fence.
//
// The two caches are separate and a pipeline can reach one without the other
// — an mcp-backed resource TYPE is checked here, an agent's tool grant there
// — so a fix applied to one leaves the identical bug live in the other: a
// daemon that keeps refusing a resource check for the rest of the cache
// window over a login that has already been completed.
func TestListToolsDoesNotCacheAServerThatNeedsALogin(t *testing.T) {
	// Not t.Parallel(): t.Setenv, and toolsCache is process-wide state.
	ResetPreflightCache()

	dir := t.TempDir()
	// Both, to cover darwin ($HOME-based) and linux (XDG-based)
	// os.UserConfigDir(), which is what TokenPath resolves through.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	endpoint := mcpFixtureServer(t).URL

	cfg := &config.Config{
		Name: "a",
		MCPServers: []config.MCPServer{{
			Name:     "test",
			Endpoint: endpoint,
			Auth:     config.MCPServerAuth{Type: "oauth"},
		}},
	}

	settings := &config.Preflight{}

	_, err := listToolsCached(context.Background(), cfg, "test", settings)
	if err == nil {
		t.Fatal("an oauth server with no token file listed its tools")
	}

	if !errors.Is(err, stepsmcp.ErrNeedsLogin) {
		t.Fatalf("listing tools on an unauthorized server: %v, want an ErrNeedsLogin", err)
	}

	authorizeFixture(t, "a", endpoint)

	tools, err := listToolsCached(context.Background(), cfg, "test", settings)
	if err != nil {
		t.Fatalf("listing tools right after the login: %v — the login was already done when this ran", err)
	}

	if len(tools) == 0 {
		t.Fatal("listed no tools from a server that exposes several")
	}
}

// TestListToolsDoesNotLetOnePipelinesLoginVouchForAnother is internal/agent's
// test of the same name, for this cache: two pipelines declaring one oauth
// server, only one logged in, must not share a verdict.
func TestListToolsDoesNotLetOnePipelinesLoginVouchForAnother(t *testing.T) {
	// Not t.Parallel(): t.Setenv, and toolsCache is process-wide state.
	ResetPreflightCache()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	endpoint := mcpFixtureServer(t).URL
	authorizeFixture(t, "a", endpoint)

	servers := []config.MCPServer{{Name: "test", Endpoint: endpoint, Auth: config.MCPServerAuth{Type: "oauth"}}}
	settings := &config.Preflight{}

	_, err := listToolsCached(context.Background(), &config.Config{Name: "a", MCPServers: servers}, "test", settings)
	if err != nil {
		t.Fatalf("the logged-in pipeline: %v", err)
	}

	_, err = listToolsCached(context.Background(), &config.Config{Name: "b", MCPServers: servers}, "test", settings)
	if !errors.Is(err, stepsmcp.ErrNeedsLogin) {
		t.Fatalf("a pipeline with no login of its own: %v, want an ErrNeedsLogin", err)
	}
}

// authorizeFixture writes the token file a finished `steps mcp login` leaves
// behind: the endpoint it was issued for (checkCredential refuses a file
// belonging to a different one) and an access token with room left on it.
func authorizeFixture(t *testing.T, pipeline, endpoint string) {
	t.Helper()

	path, err := stepsmcp.TokenPath(pipeline, "test")
	if err != nil {
		t.Fatal(err)
	}

	tf := &stepsmcp.TokenFile{
		Endpoint:     endpoint,
		ClientID:     "client",
		TokenURL:     endpoint + "/token",
		AccessToken:  "access",
		RefreshToken: "refresh",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatalf("saving the token file a login would have written: %v", err)
	}
}
