package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RecordRevision interns the configuration this handle's runs were started
// from, and makes it the one StartRun pins onto every run from here on.
//
// Interning only: it does NOT decide what the next run records. That was the
// original design — the id lived on this handle and StartRun read it — and it
// was wrong in a way the handle could not see. A run takes its configuration
// as a pointer and writes its row minutes later, after placement, leases,
// image pulls and preflight; a reload in that window made the run name a
// configuration it never executed. The sha travels WITH the config instead
// (see revisionBySHA).
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

	return nil
}

// Revision is one recorded configuration: the substituted source a run was
// started from, and the hash that identifies it.
type Revision struct {
	SHA    string
	Source string
}

// FindRevision returns a configuration this pipeline has run, by its hash.
//
// Scoped to this pipeline like everything else here, even though the hash
// alone would find the row: a state file may hold several pipelines, and one
// answering for another's configuration would be a page showing a file the
// reader's pipeline never ran.
func (s *Store) FindRevision(ctx context.Context, sha string) (Revision, bool, error) {
	var rev Revision

	err := s.db.QueryRowContext(ctx, `
		SELECT sha, source FROM pipeline_revisions
		WHERE pipeline_id = ? AND sha = ?
	`, s.pipelineID, sha).Scan(&rev.SHA, &rev.Source)

	if errors.Is(err, sql.ErrNoRows) {
		return Revision{}, false, nil
	}

	if err != nil {
		return Revision{}, false, fmt.Errorf("could not read configuration %q of pipeline %q: %w", sha, s.pipeline, err)
	}

	return rev, true, nil
}

// PruneRevisions drops the configurations nothing points at any more.
//
// Its own entry point as well as part of a run prune, because the two
// orphaning events are different: runs are reaped when a job passes its cap,
// and a configuration is orphaned either by that or by a reload superseding
// one nothing ever ran. An operator iterating on a pipeline with `steps web`
// running mints a multi-kilobyte row per distinct save, and waiting for a job
// to pass run_history: before reclaiming any of them is not a bound.
func (s *Store) PruneRevisions(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not prune configurations: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	pruned, err := pruneRevisions(ctx, tx, s.pipelineID)
	if err != nil {
		return err
	}

	if !pruned {
		return nil
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not prune configurations: %w", err)
	}

	return nil
}

// pruneRevisions is the statement both entry points share. It reports whether
// anything went, because its caller decides whether there is a transaction
// worth committing.
//
// Runs first, then this: the RESTRICT on runs.revision_id makes that order a
// rule the database keeps rather than a convention this function remembers.
//
// The NEWEST revision is exempt however few runs reference it, and that is
// not a nicety. It is the one a run admitted a moment from now will name: a
// daemon that reloads and then prunes — because a build that started under
// the previous configuration just finished — would otherwise reap the row its
// next run is about to point at, and that run would record no configuration
// at all. Newest rather than a "current" the handle tracks, because the
// newest row IS the most recently loaded one, and a rule the table can state
// about itself cannot fall out of step with a field somewhere else.
func pruneRevisions(ctx context.Context, tx *sql.Tx, pipelineID int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM pipeline_revisions
		WHERE pipeline_id = ?
		  AND id != (SELECT MAX(id) FROM pipeline_revisions WHERE pipeline_id = ?)
		  AND id NOT IN (SELECT revision_id FROM runs WHERE revision_id IS NOT NULL)
	`, pipelineID, pipelineID)
	if err != nil {
		return false, fmt.Errorf("could not prune configurations: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not prune configurations: %w", err)
	}

	return deleted > 0, nil
}
