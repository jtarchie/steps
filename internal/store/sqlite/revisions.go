package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordRevision interns the configuration a run was started from.
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
// thousand times is one row. The conflict refreshes loaded_at and nothing
// else: source is the same bytes by definition — the sha is over them, so
// rewriting a multi-KB column would be a WAL page per load for no change —
// while loaded_at is what the sweep reads to know which row is the one being
// served, and a conflict is precisely a re-load.
func (s *Store) RecordRevision(ctx context.Context, sha, source string, includes map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
	}

	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO pipeline_revisions (pipeline_id, sha, source, loaded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (pipeline_id, sha) DO UPDATE SET loaded_at = excluded.loaded_at
	`, s.pipelineID, sha, source, nowNano())
	if err != nil {
		return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
	}

	// The sha is over the includes too, so a conflicting row already holds byte-identical ones and INSERT OR IGNORE is exact.
	for path, content := range includes {
		_, err = tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO revision_includes (revision_id, path, content)
			SELECT id, ?, ? FROM pipeline_revisions WHERE pipeline_id = ? AND sha = ?
		`, path, content, s.pipelineID, sha)
		if err != nil {
			return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("could not record the configuration of pipeline %q: %w", s.pipeline, err)
	}

	return nil
}

// SetCurrentRevision refuses a sha nothing recorded rather than pointing at nothing: a daemon restarting against a NULL would silently serve no pipeline. The path rides in the same UPDATE, because a second write after it could fail with the switch already made.
func (s *Store) SetCurrentRevision(ctx context.Context, sha, from string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE pipelines
		SET current_revision_id = (SELECT id FROM pipeline_revisions WHERE pipeline_id = ? AND sha = ?),
		    path = ?, set_at = ?
		WHERE id = ? AND EXISTS (SELECT 1 FROM pipeline_revisions WHERE pipeline_id = ? AND sha = ?)
	`, s.pipelineID, sha, from, nowNano(), s.pipelineID, s.pipelineID, sha)
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
	var sha sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT r.sha FROM pipelines p
		JOIN pipeline_revisions r ON r.id = p.current_revision_id
		WHERE p.id = ?
	`, s.pipelineID).Scan(&sha)

	if errors.Is(err, sql.ErrNoRows) || (err == nil && !sha.Valid) {
		return store.Revision{}, false, nil
	}

	if err != nil {
		return store.Revision{}, false, fmt.Errorf("could not read the configuration of pipeline %q: %w", s.pipeline, err)
	}

	return s.FindRevision(ctx, sha.String)
}

// FindRevision returns a configuration this pipeline has run, by its hash.
//
// Scoped to this pipeline like everything else here, even though the hash
// alone would find the row: a state file may hold several pipelines, and one
// answering for another's configuration would be a page showing a file the
// reader's pipeline never ran.
func (s *Store) FindRevision(ctx context.Context, sha string) (store.Revision, bool, error) {
	var (
		rev store.Revision
		id  int64
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT id, sha, source FROM pipeline_revisions
		WHERE pipeline_id = ? AND sha = ?
	`, s.pipelineID, sha).Scan(&id, &rev.SHA, &rev.Source)

	if errors.Is(err, sql.ErrNoRows) {
		return store.Revision{}, false, nil
	}

	if err != nil {
		return store.Revision{}, false, fmt.Errorf("could not read configuration %q of pipeline %q: %w", sha, s.pipeline, err)
	}

	includes, err := collect(ctx, s.db, "included files", `
		SELECT path, content FROM revision_includes WHERE revision_id = ?
	`, []any{id}, func(rows *sql.Rows) ([2]string, error) {
		var pair [2]string

		err := rows.Scan(&pair[0], &pair[1])

		return pair, err //nolint:wrapcheck // collect wraps it with the query's own context
	})
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

// pruneAllRevisions drops the configurations nothing points at any more.
//
// Reached from a zero Retention as well as from a run prune, because the two
// orphaning events are different: runs are reaped when a job passes its cap,
// and a configuration is orphaned either by that or by a reload superseding
// one nothing ever ran. An operator iterating on a pipeline with `steps web`
// running mints a multi-kilobyte row per distinct save, and waiting for a job
// to pass run_history: before reclaiming any of them is not a bound.
func (s *Store) pruneAllRevisions(ctx context.Context) error {
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
// The most recently LOADED revision is exempt however few runs reference it,
// and that is not a nicety. It is the one a run admitted a moment from now
// will name: a daemon that reloads and then prunes — because a build that
// started under the previous configuration just finished — would otherwise
// reap the row its next run is about to point at, and that run would record
// no configuration at all. A rule the table states about itself rather than a
// "current" the handle tracks, because the two cannot fall out of step.
//
// By loaded_at and not by id, because an upsert keeps the row it conflicts
// with: reverting an edit re-loads a configuration whose id was minted before
// the one it supersedes, so the highest id was the row NOBODY was serving and
// the exemption protected it while the served one was swept.
//
// The runs subquery is scoped like every other read here. It is one pipeline's
// history: an unscoped anti-join is only harmless while revision ids happen to
// be unique across pipelines, and it makes the end of every build consult the
// runs of pipelines that share the state file.
//
// The CURRENT revision — the one a `steps pipeline set` made this pipeline's
// — is exempt too, and the schema's RESTRICT on pipelines.current_revision_id
// is what turns forgetting it here into an error rather than a daemon that
// restarts into nothing.
func pruneRevisions(ctx context.Context, tx *sql.Tx, pipelineID int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `
		DELETE FROM pipeline_revisions
		WHERE pipeline_id = ?
		  AND id != (
		      SELECT id FROM pipeline_revisions WHERE pipeline_id = ?
		      ORDER BY loaded_at DESC, id DESC LIMIT 1
		  )
		  AND id IS NOT (SELECT current_revision_id FROM pipelines WHERE id = ?)
		  AND id NOT IN (
		      SELECT revision_id FROM runs
		      WHERE pipeline_id = ? AND revision_id IS NOT NULL
		  )
	`, pipelineID, pipelineID, pipelineID, pipelineID)
	if err != nil {
		return false, fmt.Errorf("could not prune configurations: %w", err)
	}

	deleted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("could not prune configurations: %w", err)
	}

	return deleted > 0, nil
}
