package cli

// The credential cache is validated by the token FILE, and these are the
// transitions that would make a clock-based one wrong. None of them can be
// parallel: the token directory is process environment.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// The endpoint every test here starts from; one moves it, to prove the cache notices.
const trackerEndpoint = "https://tracker.example/mcp"

const oauthServerPipeline = `
mcp_servers:
- name: tracker
  endpoint: ENDPOINT
  auth: { type: oauth }
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`

// countingDaemon is a daemon whose credential reads are counted, which is the
// only way to tell a cache that works from one that is merely correct.
func countingDaemon(t *testing.T) (*daemon, *int) {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("HOME", dir)

	held := servingDaemon(t)
	setPipeline(t, held, "app", strings.Replace(oauthServerPipeline, "ENDPOINT", trackerEndpoint, 1))

	reads := 0
	inner := held.creds.inspect
	held.creds.inspect = func(srv config.MCPServer) stepsmcp.TokenState {
		reads++

		return inner(srv)
	}

	return held, &reads
}

func credentialOf(t *testing.T, held *daemon) web.MCPCredential {
	t.Helper()

	return held.MCPState(held.server.Lookup("app"), "tracker").Credential
}

func liveToken() *stepsmcp.TokenFile {
	return &stepsmcp.TokenFile{Endpoint: trackerEndpoint, AccessToken: secretToken, RefreshToken: "r"}
}

// TestASavedTokenIsReadOnceUntilTheFileChanges: the whole point. The header
// asks this for every oauth server of every served pipeline on every render,
// so a second ask against an unchanged file must cost a stat and nothing else.
func TestASavedTokenIsReadOnceUntilTheFileChanges(t *testing.T) {
	held, reads := countingDaemon(t)
	writeTokenFile(t, liveToken())

	for range 5 {
		if got := credentialOf(t, held); !got.Connected {
			t.Fatalf("credential = %+v, want connected", got)
		}
	}

	if *reads != 1 {
		t.Errorf("read the token file %d times for 5 renders, want 1", *reads)
	}
}

// TestAServerNobodyLoggedIntoIsAlsoReadOnce: the ABSENT file is the steady
// state of most declared servers, and the badge asks about it just as often.
// Caching only the answers that happen to exist leaves the common case paying
// full price on every render.
func TestAServerNobodyLoggedIntoIsAlsoReadOnce(t *testing.T) {
	held, reads := countingDaemon(t)

	for range 5 {
		if got := credentialOf(t, held); got.Connected {
			t.Fatalf("credential = %+v with no token saved, want not connected", got)
		}
	}

	if *reads != 1 {
		t.Errorf("looked for the token file %d times for 5 renders, want 1", *reads)
	}
}

// TestALoginIsVisibleImmediately is the interaction a TTL would sit on: a
// reader comes back from a consent screen and the page has to stop saying
// "needs login" on the very next render, not whenever a timer says so.
func TestALoginIsVisibleImmediately(t *testing.T) {
	held, _ := countingDaemon(t)

	if got := credentialOf(t, held); got.Connected {
		t.Fatalf("credential = %+v before any login, want not connected", got)
	}

	writeTokenFile(t, liveToken())

	if got := credentialOf(t, held); !got.Connected {
		t.Errorf("credential = %+v right after a login, want connected", got)
	}
}

// TestATokenGoingAwayIsVisibleImmediately: the other direction, which is what
// a `steps mcp logout` or a hand-deleted file looks like.
func TestATokenGoingAwayIsVisibleImmediately(t *testing.T) {
	held, _ := countingDaemon(t)
	writeTokenFile(t, liveToken())

	if got := credentialOf(t, held); !got.Connected {
		t.Fatalf("credential = %+v, want connected", got)
	}

	writeTokenFile(t, nil)

	if got := credentialOf(t, held); got.Connected {
		t.Errorf("credential = %+v after the token was removed, want not connected", got)
	}
}

// TestMovingTheEndpointRereadsTheToken: the token file is keyed by NAME, so a
// `steps pipeline set` that moves the endpoint turns a connected server into
// an "authorized for a different endpoint" one without touching the file. A
// cache keyed on the file alone would keep calling it connected.
func TestMovingTheEndpointRereadsTheToken(t *testing.T) {
	held, _ := countingDaemon(t)
	writeTokenFile(t, liveToken())

	if got := credentialOf(t, held); !got.Connected {
		t.Fatalf("credential = %+v, want connected", got)
	}

	setPipeline(t, held, "app", strings.Replace(oauthServerPipeline, "ENDPOINT", "https://moved.example/mcp", 1))

	if got := credentialOf(t, held); got.Connected {
		t.Errorf("credential = %+v after the endpoint moved, want not connected", got)
	}
}

// TestACachedCredentialDoesNotOutliveItsBound: an access token with no refresh
// token expires while the file sits perfectly still, which is the one change a
// stat cannot report. The bound is the backstop, so an entry past it re-reads.
func TestACachedCredentialDoesNotOutliveItsBound(t *testing.T) {
	held, reads := countingDaemon(t)
	writeTokenFile(t, liveToken())

	credentialOf(t, held)

	held.creds.mu.Lock()
	aged := held.creds.entries["tracker"]
	aged.readAt = time.Now().Add(-credentialMaxAge - time.Second)
	held.creds.entries["tracker"] = aged
	held.creds.mu.Unlock()

	credentialOf(t, held)

	if *reads != 2 {
		t.Errorf("read the token file %d times, want the aged entry to have been re-read", *reads)
	}
}
