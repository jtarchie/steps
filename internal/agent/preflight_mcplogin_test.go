package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
)

// TestPreflightDoesNotCacheAServerThatNeedsALogin is the daemon that
// disbelieved a login it had just been given.
//
// `steps web` polled a pipeline whose oauth server had no token yet, and the
// failure — decided by opening a file that was not there — went into
// probeCache like any other. Thirty seconds later the operator finished the
// login in the browser tab this same daemon was serving; the token landed on
// disk and the mcp tab said authorized. Every poll for the next five minutes
// still failed, quoting the stat of a file that by then existed. Nothing
// invalidated the entry (ResetProbeCache has no caller outside tests), so the
// only cure was restarting the process that had just been told the answer.
//
// The cache window is deliberately left at its default here: shortening it
// would hide the bug rather than test it, and the fix is that this class of
// verdict is never stored at all.
func TestPreflightDoesNotCacheAServerThatNeedsALogin(t *testing.T) {
	// Not t.Parallel(): t.Setenv, and probeCache is process-wide state.
	ResetProbeCache()

	dir := t.TempDir()
	// Both, to cover darwin ($HOME-based) and linux (XDG-based)
	// os.UserConfigDir(), which is what TokenPath resolves through.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	mcp := newCountingMCPServer(t)
	endpoint := mcp.ts.URL + "/mcp"

	cfg := &config.Config{
		Name: "a",
		MCPServers: []config.MCPServer{{
			Name:     "test",
			Endpoint: endpoint,
			Auth:     config.MCPServerAuth{Type: "oauth"},
		}},
	}

	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	err := probeServerCached(context.Background(), cfg, spec, settings)
	if err == nil {
		t.Fatal("an oauth server with no token file passed preflight")
	}

	if !errors.Is(err, stepsmcp.ErrNeedsLogin) {
		t.Fatalf("probing an unauthorized server: %v, want an ErrNeedsLogin", err)
	}

	authorize(t, "a", endpoint)

	// Immediately, well inside the cache window: this is the poll that used
	// to replay the stale verdict.
	err = probeServerCached(context.Background(), cfg, spec, settings)
	if err != nil {
		t.Fatalf("probing right after the login: %v — the login was already done when this ran", err)
	}
}

// TestPreflightStillCachesAnAuthorizedServer is the other half: declining to
// remember the unauthorized verdict must not cost the cache its reason to
// exist. Once a token is on disk, two probes still make one connection —
// which is what keeps a long-running watcher from paying a round trip per
// poll.
func TestPreflightStillCachesAnAuthorizedServer(t *testing.T) {
	// Not t.Parallel(): t.Setenv, and probeCache is process-wide state.
	ResetProbeCache()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	mcp := newCountingMCPServer(t)
	endpoint := mcp.ts.URL + "/mcp"

	authorize(t, "a", endpoint)

	cfg := &config.Config{
		Name: "a",
		MCPServers: []config.MCPServer{{
			Name:     "test",
			Endpoint: endpoint,
			Auth:     config.MCPServerAuth{Type: "oauth"},
		}},
	}

	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	for range 2 {
		err := probeServerCached(context.Background(), cfg, spec, settings)
		if err != nil {
			t.Fatalf("probeServerCached: %v", err)
		}
	}

	if *mcp.listCalls != 1 {
		t.Fatalf("tools/list called %d times for an authorized server, want 1 — the verdict is still cached", *mcp.listCalls)
	}
}

// TestPreflightDoesNotLetOnePipelinesLoginVouchForAnother is two pipelines
// declaring the same oauth server, only one of them logged in. The cache is
// process-wide and keyed by the server's definition, which both share, so
// without the pipeline in the key the logged-in one's pass answered for the
// other, and the other's run then failed later, inside the step.
func TestPreflightDoesNotLetOnePipelinesLoginVouchForAnother(t *testing.T) {
	// Not t.Parallel(): t.Setenv, and probeCache is process-wide state.
	ResetProbeCache()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)

	mcp := newCountingMCPServer(t)
	endpoint := mcp.ts.URL + "/mcp"

	authorize(t, "a", endpoint)

	servers := []config.MCPServer{{Name: "test", Endpoint: endpoint, Auth: config.MCPServerAuth{Type: "oauth"}}}
	spec := config.ToolSpec{MCP: "test", MCPTool: "search_issues"}
	settings := &config.Preflight{}

	err := probeServerCached(context.Background(), &config.Config{Name: "a", MCPServers: servers}, spec, settings)
	if err != nil {
		t.Fatalf("the logged-in pipeline: %v", err)
	}

	err = probeServerCached(context.Background(), &config.Config{Name: "b", MCPServers: servers}, spec, settings)
	if !errors.Is(err, stepsmcp.ErrNeedsLogin) {
		t.Fatalf("a pipeline with no login of its own: %v, want an ErrNeedsLogin", err)
	}
}

// authorize writes the token file a finished `steps mcp login` leaves behind,
// which is the whole of what the browser half of that flow contributes here:
// the endpoint it was issued for (checkCredential refuses a file belonging to
// a different one) and an access token with room left on it.
func authorize(t *testing.T, pipeline, endpoint string) {
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
