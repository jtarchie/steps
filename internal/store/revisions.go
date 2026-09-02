package store

import (
	"context"
	"database/sql"
	"fmt"
)

// RecordRevision interns the configuration this handle's runs were started
// from, and makes it the one StartRun pins onto every run from here on.
//
// Held on the handle rather than passed to StartRun for the reason the
// pipeline id is (see Store): the revision is a property of the configuration
// this process loaded, and a caller that has to remember to pass it is a
// caller that can forget — leaving a run recording no configuration at all,
// which is indistinguishable from a run that legitimately had none.
//
// Upsert by (pipeline_id, sha), so an unchanged configuration loaded a
// thousand times is one row. DO UPDATE rather than DO NOTHING on the
// conflict, because RETURNING wants a row: source is written back to itself,
// which is the same bytes by definition — the sha is over them.
func (s *Store) RecordRevision(ctx context.Context, sha, source string) error {
	var id int64

	err := s.db.QueryRowContext(ctx, `
		INSERT INTO pipeline_revisions (pipeline_id, sha, source, loaded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (pipeline_id, sha) DO UPDATE SET source = excluded.source
		RETURNING id
	`, s.pipelineID, sha, source, nowNano()).Scan(&id)
	if err != nil {
		return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
	}

	s.revisionID.Store(id)

	return nil
}

// pruneRevisions drops the configurations nothing points at any more.
//
// Runs first, then this: the RESTRICT on runs.revision_id makes that order a
// rule the database keeps rather than a convention this function remembers.
//
// current is exempt however few runs reference it — a handle that has loaded
// a configuration and not yet built anything with it would otherwise reap the
// row its next run is about to pin. That is what the Store holding the id
// buys, and why this cannot be a standalone sweep over the file.
func pruneRevisions(ctx context.Context, tx *sql.Tx, pipelineID, current int64, deletedRuns bool) error {
	// Only when a run actually went: a configuration loses its last reference
	// when the runs holding it are reaped, so an untouched history cannot
	// have orphaned one. The other way to orphan a revision — loading a
	// configuration and swapping away from it without ever building — is the
	// swap's to clean up, since nothing reaps at all until a build ends.
	if !deletedRuns {
		return nil
	}

	_, err := tx.ExecContext(ctx, `
		DELETE FROM pipeline_revisions
		WHERE pipeline_id = ?
		  AND id != ?
		  AND id NOT IN (SELECT revision_id FROM runs WHERE revision_id IS NOT NULL)
	`, pipelineID, current)
	if err != nil {
		return fmt.Errorf("could not prune configurations: %w", err)
	}

	return nil
}

// currentRevision is what StartRun writes: the recorded configuration, or SQL
// NULL when nothing has recorded one. Nil rather than 0 because 0 is not a
// row id and the foreign key would refuse it — a run with no configuration
// points at nothing, which is what NULL means.
func (s *Store) currentRevision() any {
	id := s.revisionID.Load()
	if id == 0 {
		return nil
	}

	return id
}
