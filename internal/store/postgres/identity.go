package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// Identity is the sqlite driver's Identity: minted on first ask, kept with the schema.
func Identity(ctx context.Context, rawURL string) (string, error) {
	held, err := acquirePool(rawURL)
	if err != nil {
		return "", err
	}

	defer func() {
		if held.release() {
			_ = held.db.Close()
		}
	}()

	var owner string

	err = inSchema(ctx, held.connection, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO identity (singleton, owner) VALUES (1, $1) ON CONFLICT (singleton) DO NOTHING`, store.MintIdentity())
		if err != nil {
			return fmt.Errorf("could not mint the identity: %w", err)
		}

		err = tx.QueryRowContext(ctx, `SELECT owner FROM identity WHERE singleton = 1`).Scan(&owner)
		if err != nil {
			return fmt.Errorf("could not read the identity: %w", err)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return owner, nil
}
