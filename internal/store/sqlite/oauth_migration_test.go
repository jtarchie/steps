package sqlite

import (
	"path/filepath"
	"testing"
)

// v010Grants are grants v0.1.0 kept by pipeline NAME: one on a pipeline the file holds, one on a pipeline long destroyed.
const v010Grants = `
INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES ('claude', 'claude', '[]', '2026-10-01T12:00:00.000000000Z');
INSERT INTO oauth_tokens (token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at)
    VALUES ('kept', 'access', 'claude', 'github:alex', 'app', 'support', '2026-10-01T12:00:00.000000000Z', '2999-01-01T00:00:00.000000000Z'),
           ('orphan', 'access', 'claude', 'github:alex', 'gone', 'support', '2026-10-01T12:00:00.000000000Z', '2999-01-01T00:00:00.000000000Z');
`

// TestTheTokenStepMapsNamesToPipelines: a grant on a pipeline the file holds survives the upgrade on that pipeline; one on a name nothing answers to is dropped rather than left for a later pipeline of that name to inherit.
func TestTheTokenStepMapsNamesToPipelines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "old.db")
	execFile(t, path, v010+v010Grants)

	accounts, err := OpenAccounts(path)
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	defer func() { _ = accounts.Close() }()

	token, found, err := accounts.FindToken(t.Context(), "kept")
	if err != nil || !found || token.Grant.Pipeline != "app" {
		t.Errorf("FindToken(kept) = %+v, %v, %v; want it on pipeline app", token, found, err)
	}

	_, found, err = accounts.FindToken(t.Context(), "orphan")
	if err != nil || found {
		t.Errorf("FindToken(orphan) = %v, %v; want its grant on a destroyed pipeline gone", found, err)
	}

	if got := queryStrings(t, path, "SELECT token_hash FROM oauth_tokens"); len(got) != 1 {
		t.Errorf("oauth_tokens after the upgrade = %v, want only the mapped row", got)
	}
}
