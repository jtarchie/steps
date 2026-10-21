package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Vault is where a pipeline's oauth logins are kept. internal/cli installs the one that seals them into the state database; this package hands it plaintext and never decides where it lands.
type Vault interface {
	// Load is the login saved for server in pipeline, and scope names the pipeline it was read under: stable across a rename, different for a pipeline later set under a destroyed one's name. found is false when there is none.
	Load(ctx context.Context, pipeline, server string) (data []byte, scope string, found bool, err error)
	// Save replaces it. A non-empty scope is a write-back, refused with ErrLoginOrphaned unless the pipeline is still the one the login was read under; empty is a fresh login for whichever pipeline holds the name. A vault that cannot keep it safe refuses rather than keep it in the clear.
	Save(ctx context.Context, pipeline, server, scope string, data []byte) error
}

// ErrLoginOrphaned is a write-back for a pipeline that is gone: destroyed, or its name taken by another since the login was read.
var ErrLoginOrphaned = errors.New("the pipeline this login was read under is gone")

var errNoVault = errors.New("mcp: this command keeps no oauth logins (no credential store installed)")

//nolint:gochecknoglobals // the process's one credential store, installed by internal/cli, as the config directory was before it
var installed struct {
	mu    sync.RWMutex
	vault Vault
}

// UseVault installs v for this process and returns what it replaced, so a test can put it back. The tokens held from the old one are dropped with it.
func UseVault(v Vault) Vault {
	installed.mu.Lock()
	defer installed.mu.Unlock()

	sharedTokens.mu.Lock()
	clear(sharedTokens.byKey)
	sharedTokens.mu.Unlock()

	previous := installed.vault
	installed.vault = v

	return previous
}

func currentVault() (Vault, error) {
	installed.mu.RLock()
	defer installed.mu.RUnlock()

	if installed.vault == nil {
		return nil, errNoVault
	}

	return installed.vault, nil
}

// loadLogin is the login saved for server, or nil when there is none.
func loadLogin(ctx context.Context, pipeline, server string) (*TokenFile, error) {
	vault, err := currentVault()
	if err != nil {
		return nil, err
	}

	data, scope, found, err := vault.Load(ctx, pipeline, server)
	if err != nil {
		return nil, fmt.Errorf("read the login: %w", err)
	}

	if !found {
		return importLegacyLogin(ctx, vault, pipeline, server)
	}

	var tf TokenFile

	err = json.Unmarshal(data, &tf)
	if err != nil {
		return nil, fmt.Errorf("parse the login: %w", err)
	}

	tf.scope = scope

	return &tf, nil
}

func saveLogin(ctx context.Context, pipeline, server string, tf *TokenFile) error {
	vault, err := currentVault()
	if err != nil {
		return err
	}

	data, err := json.Marshal(tf) //nolint:gosec // G117: marshalled to be handed to the vault, which seals it; see TokenFile
	if err != nil {
		return fmt.Errorf("marshal the login: %w", err)
	}

	err = vault.Save(ctx, pipeline, server, tf.scope, data)
	if err != nil {
		return fmt.Errorf("save the login: %w", err)
	}

	return nil
}

// importLegacyLogin moves a login an older steps left in the config directory into the vault, the first time anything asks for it, and removes the file once the vault holds it.
// ponytail: a migration with no end date; delete it, TokenPath and the *Logins functions a few releases after the one that ships it.
func importLegacyLogin(ctx context.Context, vault Vault, pipeline, server string) (*TokenFile, error) {
	path, err := TokenPath(pipeline, server)
	if err != nil {
		return nil, nil //nolint:nilnil,nilerr // no config directory, so no older login to import: simply none
	}

	tf, err := LoadTokenFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // the common case: no login at all
	}

	if err != nil {
		return nil, err
	}

	data, err := json.Marshal(tf) //nolint:gosec // G117: handed to the vault, which seals it
	if err != nil {
		return nil, fmt.Errorf("marshal the login: %w", err)
	}

	err = vault.Save(ctx, pipeline, server, "", data)
	if err != nil {
		return nil, fmt.Errorf("found a login an older steps saved at %s, which moves into the state database on first use: %w", path, err)
	}

	err = os.Remove(path)
	if err != nil {
		slog.WarnContext(ctx, "mcp.login.import_left_file", "path", path, "error", err,
			"hint", "the login is in the state database now; delete the file, which still holds it in the clear")
	}

	_ = os.Remove(filepath.Dir(path))

	slog.InfoContext(ctx, "mcp.login.imported", "pipeline", pipeline, "server", server, "from", path)

	return tf, nil
}

// MemoryVault keeps logins in memory, for tests that are not about where they land.
type MemoryVault struct {
	mu     sync.Mutex
	logins map[string][]byte
}

// Load is the login saved for pipeline's server; a MemoryVault has one scope.
func (m *MemoryVault) Load(_ context.Context, pipeline, server string) ([]byte, string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, found := m.logins[pipeline+"\x00"+server]

	return data, "", found, nil
}

// Save replaces it.
func (m *MemoryVault) Save(_ context.Context, pipeline, server, _ string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.logins == nil {
		m.logins = map[string][]byte{}
	}

	m.logins[pipeline+"\x00"+server] = append([]byte(nil), data...)

	return nil
}

// Put saves tf as a login for pipeline's server.
func (m *MemoryVault) Put(pipeline, server string, tf *TokenFile) error {
	data, err := json.Marshal(tf) //nolint:gosec // G117: a test fixture's invented token
	if err != nil {
		return fmt.Errorf("marshal the login: %w", err)
	}

	return m.Save(context.Background(), pipeline, server, "", data)
}

// Get is the login saved for pipeline's server, or nil.
func (m *MemoryVault) Get(pipeline, server string) *TokenFile {
	data, _, found, _ := m.Load(context.Background(), pipeline, server)
	if !found {
		return nil
	}

	var tf TokenFile

	if json.Unmarshal(data, &tf) != nil {
		return nil
	}

	return &tf
}
