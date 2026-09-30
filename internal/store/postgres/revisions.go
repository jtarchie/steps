package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordRevision interns the configuration a run was started from; the
// conflict refreshes loaded_at, which is what the sweep reads as "served".
func (s *Store) RecordRevision(ctx context.Context, sha, source string, includes map[string]string) error {
	err := s.write(ctx, func(tx *sql.Tx) error {
		var revisionID int64

		err := tx.QueryRowContext(ctx, `
			INSERT INTO pipeline_revisions (pipeline_id, sha, source, loaded_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (pipeline_id, sha) DO UPDATE SET loaded_at = excluded.loaded_at
			RETURNING id
		`, s.pipelineID, sha, clean(source), now()).Scan(&revisionID)
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the pipeline
		}

		// The sha covers the includes too, so a conflicting row already holds
		// byte-identical ones.
		for path, content := range includes {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO revision_includes (revision_id, path, content) VALUES ($1, $2, $3)
				ON CONFLICT (revision_id, path) DO NOTHING
			`, revisionID, path, clean(content))
			if err != nil {
				return err //nolint:wrapcheck // wrapped below with the pipeline
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// SetCurrentRevision refuses a sha nothing recorded rather than pointing at
// nothing.
func (s *Store) SetCurrentRevision(ctx context.Context, sha, from string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE pipelines
		SET current_revision_id = (SELECT id FROM pipeline_revisions WHERE pipeline_id = $1 AND sha = $2),
		    path = $3, set_at = $4
		WHERE id = $1 AND EXISTS (SELECT 1 FROM pipeline_revisions WHERE pipeline_id = $1 AND sha = $2)
	`, s.pipelineID, sha, clean(from), now())
	if err != nil {
		return fmt.Errorf("could not set the configuration of pipeline %q: %w", s.pipeline, err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("could not set the configuration of pipeline %q: %w", s.pipeline, err)
	}

	if changed == 0 {
		return fmt.Errorf("could not set the configuration of pipeline %q: revision %s was never recorded", s.pipeline, sha)
	}

	return nil
}

// CurrentRevision reads the revision a set made current, includes and all.
func (s *Store) CurrentRevision(ctx context.Context) (store.Revision, bool, error) {
	var sha string

	err := s.db.QueryRowContext(ctx, `
		SELECT r.sha FROM pipelines p
		JOIN pipeline_revisions r ON r.id = p.current_revision_id
		WHERE p.id = $1
	`, s.pipelineID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Revision{}, false, nil
	}

	if err != nil {
		return store.Revision{}, false, fmt.Errorf("could not read the configuration of pipeline %q: %w", s.pipeline, err)
	}

	return s.FindRevision(ctx, sha)
}

// FindRevision returns a configuration this pipeline has run, by its hash.
func (s *Store) FindRevision(ctx context.Context, sha string) (store.Revision, bool, error) {
	var (
		rev store.Revision
		id  int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, sha, source FROM pipeline_revisions
		WHERE pipeline_id = $1 AND sha = $2
	`, s.pipelineID, sha).Scan(&id, &rev.SHA, &rev.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Revision{}, false, nil
	}

	if err != nil {
		return store.Revision{}, false, fmt.Errorf("could not read configuration %q of pipeline %q: %w", sha, s.pipeline, err)
	}

	includes, err := collect(ctx, s.db, "included files",
		`SELECT path, content FROM revision_includes WHERE revision_id = $1`, []any{id}, scanPair)
	if err != nil {
		return store.Revision{}, false, err
	}

	if len(includes) > 0 {
		rev.Includes = make(map[string]string, len(includes))
		for _, pair := range includes {
			rev.Includes[pair[0]] = pair[1]
		}
	}

	return rev, true, nil
}

// pruneRevisions drops the configurations nothing points at: not the most
// recently loaded, not the current one, and not one a surviving run names.
// Runs first, then this: the RESTRICT on runs.revision_id keeps that order.
func pruneRevisions(ctx context.Context, tx *sql.Tx, pipelineID int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM pipeline_revisions
		WHERE pipeline_id = $1
		  AND id <> (
		      SELECT id FROM pipeline_revisions WHERE pipeline_id = $1
		      ORDER BY loaded_at DESC, id DESC LIMIT 1
		  )
		  AND id IS DISTINCT FROM (SELECT current_revision_id FROM pipelines WHERE id = $1)
		  AND id NOT IN (
		      SELECT revision_id FROM runs
		      WHERE pipeline_id = $1 AND revision_id IS NOT NULL
		  )
	`, pipelineID)
	if err != nil {
		return fmt.Errorf("could not prune configurations: %w", err)
	}

	return nil
}
