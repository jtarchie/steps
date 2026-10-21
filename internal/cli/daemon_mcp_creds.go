package cli

// What a saved oauth login is worth, cached against the logins this process saved.
//
// The header's attention badge asks for every oauth server of every SERVED
// pipeline on every render, which includes each open tab's 2.5s self-poll,
// and each answer is a database read and a decryption.
//
// The cache is validated by loginSaves rather than by a clock, because every
// writer of a daemon's logins is this process: a login finishing, an x/oauth2
// refresh during a run, an older login imported. A TTL would have to be long
// to be worth anything and would then sit on the one interaction where the
// answer changing is the entire point — a reader coming back from a consent
// screen watching the page say "needs login" at them.
//
// One thing a counter cannot see, which is why an entry also has a maximum
// age: a token EXPIRES while nothing is written.

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/config"
	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/web"
)

// credentialMaxAge bounds the one change no save reports: an access
// token with no refresh token passing its expiry. A badge arriving up to this
// late for a token that died on its own is invisible; anything a person DID is
// a save, reported immediately.
const credentialMaxAge = 30 * time.Second

// credentials caches one answer per pipeline and server name.
//
// Keyed by what a login is keyed by, with the endpoint
// kept as a FIELD: folding the endpoint into the key would add an entry per
// version a `steps pipeline set` ever configured and drop none.
type credentials struct {
	mu      sync.Mutex
	entries map[string]credentialEntry
	// inspect is the seam a test counts calls on; nothing else replaces it.
	inspect func(context.Context, string, config.MCPServer) stepsmcp.TokenState
}

// credentialEntry is one answer and the saves it was read after. A server nobody has logged into is cached too: that is the common state, and an entry saying so is what stops the badge re-reading it on every poll.
type credentialEntry struct {
	endpoint string
	saves    int64
	readAt   time.Time
	state    web.MCPCredential
}

func newCredentials() *credentials {
	return &credentials{entries: map[string]credentialEntry{}, inspect: stepsmcp.InspectToken}
}

// of is what the page renders for one oauth server.
func (c *credentials) of(ctx context.Context, pipeline string, srv config.MCPServer) web.MCPCredential {
	saves := loginSaves.Load()

	c.mu.Lock()
	key := loginKey(pipeline, srv.Name)
	held, found := c.entries[key]
	c.mu.Unlock()

	if found && held.describes(srv, saves) {
		return held.state
	}

	// Outside the lock: a database read, which would otherwise serialize every server of every pipeline behind the slowest one.
	token := c.inspect(ctx, pipeline, srv)
	entry := credentialEntry{
		endpoint: srv.Endpoint,
		saves:    saves,
		readAt:   time.Now(),
		state:    web.MCPCredential{Connected: token.Connected, Detail: token.Detail},
	}

	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()

	return entry.state
}

// drop forgets every entry whose key starts with prefix, so a destroyed or renamed pipeline's entries do not accumulate.
func (c *credentials) drop(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for key := range c.entries {
		if strings.HasPrefix(key, prefix) {
			delete(c.entries, key)
		}
	}
}

// describes reports whether this entry still answers for the configuration in front of it.
//
// The endpoint is part of it because the answer depends on it: a login is
// "authorized for a different endpoint" by comparison, so a `steps pipeline
// set` that moves one turns a connected server into a broken one without
// saving anything.
func (e credentialEntry) describes(srv config.MCPServer, saves int64) bool {
	return e.endpoint == srv.Endpoint && e.saves == saves && time.Since(e.readAt) < credentialMaxAge
}
