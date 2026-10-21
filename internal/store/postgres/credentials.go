package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// Credential is the sealed bytes saved under name for this pipeline.
func (s *Store) Credential(ctx context.Context, name string) ([]byte, bool, error) {
	var sealed []byte

	err := s.db.QueryRowContext(ctx,
		`SELECT sealed FROM credentials WHERE pipeline_id = $1 AND name = $2`, s.pipelineID, clean(name)).Scan(&sealed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("could not read credential %q of %q: %w", name, s.pipeline, err)
	}

	return sealed, true, nil
}

// SaveCredential replaces the sealed bytes saved under name.
func (s *Store) SaveCredential(ctx context.Context, name string, sealed []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO credentials (pipeline_id, name, sealed) VALUES ($1, $2, $3)
		ON CONFLICT (pipeline_id, name) DO UPDATE SET sealed = excluded.sealed
	`, s.pipelineID, clean(name), sealed)
	if err != nil {
		return fmt.Errorf("could not save credential %q of %q: %w", name, s.pipeline, err)
	}

	return nil
}

// CredentialScope is the pipeline's id, which a rename keeps and an identity column never hands out twice.
func (s *Store) CredentialScope() string { return strconv.FormatInt(s.pipelineID, 10) }
