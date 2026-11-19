package postgres

import (
	"testing"
)

// v010Grants are grants v0.1.0 kept by pipeline NAME: one on a pipeline the schema holds, one on a pipeline long destroyed.
const v010Grants = `
INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES ('claude', 'claude', '[]', '2026-10-01T12:00:00Z');
INSERT INTO oauth_tokens (token_hash, kind, client_id, actor, pipeline, export, created_at, expires_at)
    VALUES ('kept', 'access', 'claude', 'github:alex', 'app', 'support', '2026-10-01T12:00:00Z', '2999-01-01T00:00:00Z'),
           ('orphan', 'access', 'claude', 'github:alex', 'gone', 'support', '2026-10-01T12:00:00Z', '2999-01-01T00:00:00Z');
`

// TestTheTokenStepMapsNamesToPipelines: as sqlite's — a grant on a pipeline the schema holds survives on that pipeline; one on a name nothing answers to is dropped.
func TestTheTokenStepMapsNamesToPipelines(t *testing.T) {
	t.Parallel()

	rawURL := loadV010(t, v010Grants)

	accounts, err := OpenAccounts(rawURL)
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

	if got := queryInt(t, rawDB(t, rawURL), `SELECT COUNT(*) FROM steps.oauth_tokens`); got != 1 {
		t.Errorf("oauth_tokens after the upgrade holds %d rows, want only the mapped one", got)
	}
}
