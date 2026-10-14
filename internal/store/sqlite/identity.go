package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/store"
)

// Identity is the database's own id, minted on first ask and kept: a daemon names itself with it on the cloud machines it starts (#120), and one daemon owns one database, so the id outlives the process where a hostname or pid does not.
func Identity(ctx context.Context, path string) (string, error) {
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return "", fmt.Errorf("could not create state directory for %q: %w", path, err)
	}

	err = createPrivate(path)
	if err != nil {
		return "", err
	}

	held, err := acquirePools(path)
	if err != nil {
		return "", err
	}

	defer func() {
		if held.release() {
			_ = held.reads.Close()
			_ = held.db.Close()
		}
	}()

	tx, err := held.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("could not open state db %q: %w", path, err)
	}

	defer func() { _ = tx.Rollback() }()

	err = initSchema(ctx, tx, path)
	if err != nil {
		return "", err
	}

	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO identity (singleton, owner) VALUES (1, ?)`, store.MintIdentity())
	if err != nil {
		return "", fmt.Errorf("could not mint the identity of %q: %w", path, err)
	}

	var owner string

	err = tx.QueryRowContext(ctx, `SELECT owner FROM identity WHERE singleton = 1`).Scan(&owner)
	if err != nil {
		return "", fmt.Errorf("could not read the identity of %q: %w", path, err)
	}

	err = tx.Commit()
	if err != nil {
		return "", fmt.Errorf("could not open state db %q: %w", path, err)
	}

	return owner, nil
}
