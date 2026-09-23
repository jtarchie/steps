package cli

// What a saved oauth token is worth, cached against the file itself.
//
// Reading one is open + read + close + a JSON parse with a time in it —
// measured at 24µs for a token that is present, against 3.6µs for one that is
// not. That was fine while the mcp tab was the only page asking: a handful of
// servers, on a page somebody had opened on purpose. The header's attention
// badge then began asking for every oauth server of every SERVED pipeline, on
// every render, which includes each open tab's 2.5s self-poll — four
// pipelines of three connected servers is 0.3ms of syscall and parse per
// render, per tab, forever.
//
// The cache is validated by the FILE rather than by a clock, because every
// writer of that file changes it: this daemon finishing a login, a `steps mcp
// login` in somebody's terminal, an x/oauth2 refresh during a run. A TTL would
// have to be long to be worth anything and would then sit on the one
// interaction where the answer changing is the entire point — a reader coming
// back from a consent screen watching the page say "needs login" at them.
//
// Two things a stat cannot see, which is why an entry also has a maximum age.
// A token EXPIRES while the file sits perfectly still; and a filesystem whose
// timestamps are coarse can report the same modification time and size for a
// rewrite inside one second — harmless in practice, since a refresh that keeps
// a token connected does not change what the page says about it, but not
// something to rely on. Nothing watches for either transition second by
// second, so the bound is generous.

import (
	"os"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// credentialMaxAge bounds the one change the file cannot report: an access
// token with no refresh token passing its expiry. A badge arriving up to this
// late for a token that died on its own is invisible; anything a person DID is
// reported by the file changing, immediately.
const credentialMaxAge = 30 * time.Second

// credentials caches one answer per server name.
//
// Keyed by NAME with the endpoint kept as a FIELD, for the reason probeResult
// gives: the token file is keyed by name alone, and folding the endpoint into
// the key would add an entry per version a `steps pipeline set` ever
// configured and drop none.
type credentials struct {
	mu      sync.Mutex
	entries map[string]credentialEntry
	// inspect is the seam a test counts calls on; nothing else replaces it.
	inspect func(config.MCPServer) stepsmcp.TokenState
}

// credentialEntry is one answer and the file it was read from. A file that is
// ABSENT is cached too — that is the common state of a server nobody has
// logged into, and an entry saying so is what stops the badge re-deriving it
// on every poll.
type credentialEntry struct {
	endpoint string
	missing  bool
	modTime  time.Time
	size     int64
	readAt   time.Time
	state    web.MCPCredential
}

func newCredentials() *credentials {
	return &credentials{entries: map[string]credentialEntry{}, inspect: stepsmcp.InspectToken}
}

// of is what the page renders for one oauth server.
func (c *credentials) of(srv config.MCPServer) web.MCPCredential {
	path, err := stepsmcp.TokenPath(srv.Name)
	if err != nil {
		// Without a path there is nothing to key an entry on, so this one is
		// answered fresh every time. It is also a machine with no config
		// directory, where nothing else works either.
		token := c.inspect(srv)

		return web.MCPCredential{Connected: token.Connected, Detail: token.Detail}
	}

	// Outside the lock: a stat is a syscall, and holding a mutex across one
	// would serialize every server of every pipeline behind the slowest disk.
	//
	// The error is dropped rather than carried: os.Stat returns a nil info
	// exactly when it fails, so the info IS the answer, and a guard on the
	// value about to be dereferenced is one nilaway can see. Carrying both
	// asks every reader — and the analyzer — to trust an invariant instead.
	info, _ := os.Stat(path)

	c.mu.Lock()
	defer c.mu.Unlock()

	if held, found := c.entries[srv.Name]; found && held.describes(srv, info) {
		return held.state
	}

	token := c.inspect(srv)
	entry := credentialEntry{
		endpoint: srv.Endpoint,
		missing:  info == nil,
		readAt:   time.Now(),
		state:    web.MCPCredential{Connected: token.Connected, Detail: token.Detail},
	}

	if info != nil {
		entry.modTime, entry.size = info.ModTime(), info.Size()
	}

	c.entries[srv.Name] = entry

	return entry.state
}

// describes reports whether this entry still answers for the configuration and
// the file in front of it.
//
// The endpoint is part of it because the answer depends on it: a token is
// "authorized for a different endpoint" by comparison, so a `steps pipeline
// set` that moves one turns a connected server into a broken one without
// touching the file.
func (e credentialEntry) describes(srv config.MCPServer, info os.FileInfo) bool {
	if e.endpoint != srv.Endpoint || time.Since(e.readAt) >= credentialMaxAge {
		return false
	}

	if info == nil {
		return e.missing
	}

	return !e.missing && e.size == info.Size() && e.modTime.Equal(info.ModTime())
}
