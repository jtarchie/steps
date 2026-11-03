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
	// Load is the login saved under key. StoredLogin.Scope names the pipeline it was read under: stable across a rename, different for a pipeline later set under a destroyed one's name. StoredLogin.Found is false when there is none.
	Load(ctx context.Context, key LoginKey) (StoredLogin, error)
	// Save replaces it. A non-empty scope is a write-back, refused with ErrLoginOrphaned unless the pipeline is still the one the login was read under; empty is a fresh login for whichever pipeline holds the name. A vault that cannot keep it safe refuses rather than keep it in the clear.
	Save(ctx context.Context, key LoginKey, scope string, data []byte) error
}

// LoginKey names one login: a pipeline's oauth login to one of its servers. A struct so the two strings cannot be passed swapped.
type LoginKey struct{ Pipeline, Server string }

// StoredLogin is what a Vault holds for a LoginKey.
type StoredLogin struct {
	Data  []byte
	Scope string
	Found bool
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
func loadLogin(ctx context.Context, key LoginKey) (*TokenFile, error) {
	vault, err := currentVault()
	if err != nil {
		return nil, err
	}

	login, err := vault.Load(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("read the login: %w", err)
	}

	if !login.Found {
		return importLegacyLogin(ctx, vault, key)
	}

	var tf TokenFile

	err = json.Unmarshal(login.Data, &tf)
	if err != nil {
		return nil, fmt.Errorf("parse the login: %w", err)
	}

	tf.scope = login.Scope

	return &tf, nil
}

func saveLogin(ctx context.Context, key LoginKey, tf *TokenFile) error {
	vault, err := currentVault()
	if err != nil {
		return err
	}

	data, err := json.Marshal(tf) //nolint:gosec // G117: marshalled to be handed to the vault, which seals it; see TokenFile
	if err != nil {
		return fmt.Errorf("marshal the login: %w", err)
	}

	err = vault.Save(ctx, key, tf.scope, data)
	if err != nil {
		return fmt.Errorf("save the login: %w", err)
	}

	return nil
}

// importLegacyLogin moves a login an older steps left in the config directory into the vault, the first time anything asks for it, and removes the file once the vault holds it.
// ponytail: deletable once no pre-vault login directory can still exist on any machine; it goes with TokenPath, LoadTokenFile and the config-directory *Logins functions (LoginsExist, MoveLogins, ForgetLogins), whose live callers are in internal/cli/daemon.go.
func importLegacyLogin(ctx context.Context, vault Vault, key LoginKey) (*TokenFile, error) {
	path, err := TokenPath(key.Pipeline, key.Server)
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

	err = vault.Save(ctx, key, "", data)
	if err != nil {
		return nil, fmt.Errorf("found a login an older steps saved at %s, which moves into the state database on first use: %w", path, err)
	}

	err = os.Remove(path)
	if err != nil {
		slog.WarnContext(ctx, "mcp.login.import_left_file", "path", path, "error", err,
			"hint", "the login is in the state database now; delete the file, which still holds it in the clear")
	}

	_ = os.Remove(filepath.Dir(path))

	slog.InfoContext(ctx, "mcp.login.imported", "pipeline", key.Pipeline, "server", key.Server, "from", path)

	return tf, nil
}

// MemoryVault keeps logins in memory, for tests that are not about where they land.
type MemoryVault struct {
	mu     sync.Mutex
	logins map[LoginKey][]byte
}

// Load is the login saved for pipeline's server; a MemoryVault has one scope.
func (m *MemoryVault) Load(_ context.Context, key LoginKey) (StoredLogin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, found := m.logins[key]

	return StoredLogin{Data: data, Found: found}, nil
}

// Save replaces it.
func (m *MemoryVault) Save(_ context.Context, key LoginKey, _ string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.logins == nil {
		m.logins = map[LoginKey][]byte{}
	}

	m.logins[key] = append([]byte(nil), data...)

	return nil
}

// Put saves tf as a login for pipeline's server.
func (m *MemoryVault) Put(pipeline, server string, tf *TokenFile) error {
	data, err := json.Marshal(tf) //nolint:gosec // G117: a test fixture's invented token
	if err != nil {
		return fmt.Errorf("marshal the login: %w", err)
	}

	return m.Save(context.Background(), LoginKey{pipeline, server}, "", data)
}

// Get is the login saved for pipeline's server, or nil.
func (m *MemoryVault) Get(pipeline, server string) *TokenFile {
	login, _ := m.Load(context.Background(), LoginKey{pipeline, server})
	if !login.Found {
		return nil
	}

	var tf TokenFile

	if json.Unmarshal(login.Data, &tf) != nil {
		return nil
	}

	return &tf
}
