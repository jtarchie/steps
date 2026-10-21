package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/config"
)

// refusingVault stands in for one with no encryption key, and for a database that, like a real one, fails a write under a context already done.
type refusingVault struct {
	MemoryVault
	refuse error
}

func (r *refusingVault) Save(ctx context.Context, pipeline, server string, data []byte) error {
	if r.refuse != nil {
		return r.refuse
	}

	if ctx.Err() != nil {
		return fmt.Errorf("refusing vault: %w", ctx.Err())
	}

	return r.MemoryVault.Save(ctx, pipeline, server, data)
}

func useVault(t *testing.T, v Vault) {
	t.Helper()

	previous := UseVault(v)
	t.Cleanup(func() { UseVault(previous) })
}

func writeLegacyLogin(t *testing.T, server string, tf *TokenFile) string {
	t.Helper()

	path, err := TokenPath(testPipeline, server)
	if err != nil {
		t.Fatal(err)
	}

	err = tf.Save(path)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// Not t.Parallel(): the config dir and the vault are process state.
func TestALegacyLoginMovesIntoTheVaultOnFirstUse(t *testing.T) {
	useConfigDir(t)

	vault := &refusingVault{}
	useVault(t, vault)

	legacy := &TokenFile{Endpoint: inspectEndpoint, AccessToken: "older", RefreshToken: "r"}
	path := writeLegacyLogin(t, "tracker", legacy)

	tf, err := loadLogin(t.Context(), testPipeline, "tracker")
	if err != nil || tf == nil || tf.AccessToken != legacy.AccessToken {
		t.Fatalf("loadLogin over a legacy file = %+v, %v; want it imported", tf, err)
	}

	if got := vault.Get(testPipeline, "tracker"); got == nil || got.AccessToken != legacy.AccessToken {
		t.Errorf("the vault holds %+v after the import, want the legacy login", got)
	}

	_, err = os.Stat(path)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plaintext file survived its import: %v", err)
	}
}

// Not t.Parallel(): the config dir and the vault are process state.
func TestALegacyLoginTheVaultRefusesStaysWhereItWas(t *testing.T) {
	useConfigDir(t)

	refusal := errors.New("no encryption key: set STEPS_ENCRYPTION_KEY")
	useVault(t, &refusingVault{refuse: refusal})

	path := writeLegacyLogin(t, "tracker", &TokenFile{Endpoint: inspectEndpoint, AccessToken: "a"})

	_, err := loadLogin(t.Context(), testPipeline, "tracker")
	if !errors.Is(err, refusal) || !strings.Contains(err.Error(), path) {
		t.Fatalf("loadLogin with a vault that refuses = %v, want the refusal naming %s", err, path)
	}

	_, err = os.Stat(path)
	if err != nil {
		t.Errorf("a refused import deleted the only copy of the login: %v", err)
	}

	state := InspectToken(t.Context(), testPipeline, inspectServer())
	if state.Connected || !strings.Contains(state.Detail, "STEPS_ENCRYPTION_KEY") {
		t.Errorf("the page says %+v, want it to name the missing key rather than ask for a login", state)
	}
}

// Not t.Parallel(): the vault is process state.
func TestARotatedTokenIsSavedAfterItsCallerHasGone(t *testing.T) {
	vault := &refusingVault{}
	useVault(t, vault)

	shared := &sharedToken{pipeline: testPipeline, server: "tracker", file: &TokenFile{Endpoint: inspectEndpoint, AccessToken: "old", RefreshToken: "spent"}}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	shared.persist(ctx, &oauth2.Token{AccessToken: "new", RefreshToken: "rotated"})

	if got := vault.Get(testPipeline, "tracker"); got == nil || got.RefreshToken != "rotated" {
		t.Errorf("the vault holds %+v after a refresh whose caller was cancelled, want the rotated refresh token", got)
	}
}

// Not t.Parallel(): the vault is process state.
func TestNoVaultIsNotMistakenForNoLogin(t *testing.T) {
	useVault(t, nil)

	_, err := oauthTokenSource(t.Context(), testPipeline, inspectServer())
	if !errors.Is(err, errNoVault) {
		t.Errorf("connect with no vault = %v, want errNoVault", err)
	}
}

func inspectServer() config.MCPServer {
	return config.MCPServer{Name: "tracker", Endpoint: inspectEndpoint, Auth: config.MCPServerAuth{Type: "oauth"}}
}
