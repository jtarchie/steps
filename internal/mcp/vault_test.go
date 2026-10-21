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

func (r *refusingVault) Save(ctx context.Context, pipeline, server, scope string, data []byte) error {
	if r.refuse != nil {
		return r.refuse
	}

	if ctx.Err() != nil {
		return fmt.Errorf("refusing vault: %w", ctx.Err())
	}

	return r.MemoryVault.Save(ctx, pipeline, server, scope, data)
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

// scopeVault records the scope each save carried.
type scopeVault struct {
	MemoryVault
	saved []string
}

func (v *scopeVault) Save(ctx context.Context, pipeline, server, scope string, data []byte) error {
	v.saved = append(v.saved, scope)

	return v.MemoryVault.Save(ctx, pipeline, server, scope, data)
}

// A refresh writes back to the pipeline it read from, by identity, so the vault can refuse one whose pipeline is gone.
//
// Not t.Parallel(): the vault is process state.
func TestAWriteBackCarriesTheIdentityItWasReadUnder(t *testing.T) {
	vault := &scopeVault{}
	useVault(t, vault)

	shared := &sharedToken{pipeline: testPipeline, server: "tracker", file: &TokenFile{Endpoint: inspectEndpoint, AccessToken: "old", scope: "pipeline-7"}}
	shared.persist(t.Context(), &oauth2.Token{AccessToken: "new"})

	if len(vault.saved) != 1 || vault.saved[0] != "pipeline-7" {
		t.Errorf("write-back saved under scopes %q, want the one it was read under", vault.saved)
	}
}

// fixedScopeVault answers every Load under one scope.
type fixedScopeVault struct{ scopeVault }

func (v *fixedScopeVault) Load(ctx context.Context, pipeline, server string) ([]byte, string, bool, error) {
	data, _, found, err := v.MemoryVault.Load(ctx, pipeline, server)

	return data, "pipeline-9", found, err
}

// Not t.Parallel(): the vault is process state.
func TestTheScopeALoginWasReadUnderIsTheOneItIsSavedUnder(t *testing.T) {
	vault := &fixedScopeVault{}
	useVault(t, vault)

	err := vault.Put(testPipeline, "tracker", &TokenFile{Endpoint: inspectEndpoint, AccessToken: "a"})
	if err != nil {
		t.Fatal(err)
	}

	tf, err := loadLogin(t.Context(), testPipeline, "tracker")
	if err != nil || tf == nil {
		t.Fatalf("loadLogin = %v, %v", tf, err)
	}

	vault.saved = nil

	err = saveLogin(t.Context(), testPipeline, "tracker", tf)
	if err != nil {
		t.Fatal(err)
	}

	if len(vault.saved) != 1 || vault.saved[0] != "pipeline-9" {
		t.Errorf("saved under scopes %q, want the one it was read under", vault.saved)
	}
}
