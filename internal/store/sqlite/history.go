package sqlite

// resource_versions: every version steps has seen of a resource, in the order
// it saw them.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordVersions files what a check reported, assigning each genuinely new
// version the next check_order, and prunes beyond the cap. It returns how
// many versions were new to check-history, which is what decides whether
// anything is worth triggering for.
//
// A version a check already filed is left ALONE — its check_order is the
// order it was discovered in, and revising it would reorder history under a
// job midway through walking it. A check re-reporting its whole window every
// poll therefore writes nothing, which is the common case.
//
// A version only a RUN had filed (from_check = 0) is the one exception: the
// check is discovering it now, so it takes a fresh order at the top. Keeping
// its stale order would be worse than it sounds — "latest" resolves by
// highest order, so a run-filed newest version would sort below everything a
// later check reported, and the prune would treat the newest version as the
// OLDEST and delete it first.
//
// Ordering within one call follows the slice, which is the order the check
// returned: oldest first, by the convention a check owes.
func (s *Store) RecordVersions(ctx context.Context, resourceName string, versions []map[string]any, limit int) (int, error) {
	if len(versions) == 0 {
		return 0, nil
	}

	// limit == 0 means no limit (docs/attempts-timeout.md's convention), so the
	// prune below is skipped entirely rather than falling back to the default —
	// which is what this did, making `version_history: 0` mean "cap at 1000".
	// A negative value cannot arrive from config (the schema forbids it) and is
	// treated as unset rather than as an error a storage call should invent.
	if limit < 0 {
		limit = store.DefaultResourceVersionCap
	}

	encoded, err := encodeVersions(versions)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	defer func() { _ = tx.Rollback() }()

	added, err := insertNewVersions(ctx, tx, s.pipelineID, resourceName, encoded)
	if err != nil {
		return 0, err
	}

	// Prune only what the check no longer reports. A version still in the
	// report is still real, whatever the cap says — deleting it means the
	// next poll "discovers" it again at a fresh top order, and with a cap
	// smaller than the window the table oscillates forever between halves,
	// "latest" flipping to an old version on alternate polls and every prune
	// cascading away consumed marks so jobs re-fan-out each cycle. The cap
	// therefore bounds what has scrolled AWAY, and a window larger than the
	// cap is simply kept whole.
	floor, err := minReportedOrder(ctx, tx, s.pipelineID, resourceName, encoded)
	if err != nil {
		return 0, err
	}

	if limit > 0 {
		err = pruneVersions(ctx, tx, s.pipelineID, resourceName, limit, floor)
		if err != nil {
			return 0, err
		}
	}

	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	return added, nil
}

// encodeVersions encodes once for both the upsert and the floor query, which each used to encode every version again.
func encodeVersions(versions []map[string]any) ([]string, error) {
	encoded := make([]string, 0, len(versions))

	for _, version := range versions {
		one, err := store.EncodeVersion(version)
		if err != nil {
			return nil, err //nolint:wrapcheck // the caller names the resource
		}

		encoded = append(encoded, one)
	}

	return encoded, nil
}

// insertNewVersions files the versions check-history does not hold, each
// taking the next check_order, and reports how many that was.
//
// The WHERE on the upsert is what keeps a steady-state poll free: a row a
// check already filed matches the conflict but not the WHERE, so nothing is
// written and RowsAffected is 0 — the order neither advances nor gaps. A row
// only a run had filed matches both, taking a fresh order (see
// RecordVersions).
func insertNewVersions(
	ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, encoded []string,
) (int, error) {
	next, err := nextCheckOrder(ctx, tx, pipelineID, resourceName)
	if err != nil {
		return 0, err
	}

	// Prepared once, because the driver otherwise re-prepares per row, and a check re-reporting a 1000-version window paid that 1000 times under the write lock: measured 11.9ms a poll against 2.8ms.
	upsert, err := tx.PrepareContext(ctx, `
		INSERT INTO resource_versions (pipeline_id, resource_name, version_json, check_order, from_check)
		VALUES (?, ?, ?, ?, 1)
		ON CONFLICT (pipeline_id, resource_name, version_json)
		DO UPDATE SET from_check = 1, check_order = excluded.check_order
		WHERE resource_versions.from_check = 0
	`)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	defer func() { _ = upsert.Close() }()

	added := 0

	for _, version := range encoded {
		result, err := upsert.ExecContext(ctx, pipelineID, resourceName, version, next)
		if err != nil {
			return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
		}

		changed, err := result.RowsAffected()
		if err == nil && changed > 0 {
			next++
			added++
		}
	}

	return added, nil
}

// minReportedOrder is the lowest check_order among the versions a check just
// reported — the floor below which pruning is safe.
func minReportedOrder(
	ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, encoded []string,
) (int64, error) {
	var lowest sql.NullInt64

	err := tx.QueryRowContext(ctx,
		`SELECT MIN(check_order) FROM resource_versions
		 WHERE pipeline_id = ? AND resource_name = ? AND version_json IN (SELECT value FROM json_each(?))`,
		pipelineID, resourceName, jsonList(encoded)).Scan(&lowest)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	if !lowest.Valid {
		return 1<<62 - 1, nil
	}

	return lowest.Int64, nil
}

// nextCheckOrder is the order to give the next newly-seen version.
//
// MAX+1 inside the caller's transaction rather than an AUTOINCREMENT column,
// because the sequence is PER RESOURCE: one global counter would still order
// correctly but leave gaps that make "the 50 oldest of this resource" a
// scan rather than a range. Safe against a concurrent writer because the DSN
// opens transactions IMMEDIATE, so this read already holds the write lock.
func nextCheckOrder(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string) (int64, error) {
	var highest sql.NullInt64

	err := tx.QueryRowContext(ctx,
		`SELECT MAX(check_order) FROM resource_versions WHERE pipeline_id = ? AND resource_name = ?`,
		pipelineID, resourceName).Scan(&highest)
	if err != nil {
		return 0, fmt.Errorf("could not read version order for %q: %w", resourceName, err)
	}

	if !highest.Valid {
		return 1, nil
	}

	return highest.Int64 + 1, nil
}

// pruneVersions drops the oldest versions beyond the cap, but never one at
// or above floor — the currently-reported set, which is still real however
// small the cap (see RecordVersions). The cascade takes a pruned version's
// green record with it, so nothing is left referring to a version that no
// longer exists.
func pruneVersions(
	ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName string, limit int, floor int64,
) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM resource_versions
		WHERE pipeline_id = ? AND resource_name = ? AND check_order < ? AND check_order NOT IN (
			SELECT check_order FROM resource_versions
			WHERE pipeline_id = ? AND resource_name = ?
			ORDER BY check_order DESC
			LIMIT ?
		)
	`, pipelineID, resourceName, floor, pipelineID, resourceName, limit)
	if err != nil {
		return fmt.Errorf("could not prune versions for %q: %w", resourceName, err)
	}

	return nil
}

// ResourceVersionsJSON returns the versions a CHECK has reported for a
// resource, oldest first — the same order and the same contract a check's own
// output has, so a caller can treat it as the check's answer without knowing
// where it came from.
//
// Rows that exist only because something referenced them are excluded, and an
// empty result means "nothing has checked this resource, go and check".
// Returning them would be worse than useless: a `steps run` records the one
// version it took, and treating that lone row as the resource's history would
// hide every other version from the next run.
//
// Stored JSON rather than map[string]any, because half the callers only
// DISPLAY a version and decoding every row only to re-encode it a moment later
// buys nothing. A caller that inspects fields runs DecodeVersion, which is
// where the UseNumber that keeps an id out of exponent notation lives.
func (s *Store) ResourceVersionsJSON(ctx context.Context, resourceName string) ([]string, error) {
	return collect(ctx, s.db, "resource versions",
		`SELECT version_json FROM resource_versions
		 WHERE pipeline_id = ? AND resource_name = ? AND from_check = 1
		 ORDER BY check_order`,
		[]any{s.pipelineID, resourceName}, func(rows *sql.Rows) (string, error) {
			var encoded string

			err := rows.Scan(&encoded)

			return encoded, err //nolint:wrapcheck // collect wraps with the thing being read
		})
}

// VersionOrders maps every recorded version of a resource to its
// check_order, INCLUDING the rows a check did not file.
//
// Deliberately wider than ResourceVersionsJSON, which answers "what exists" and
// therefore reports only what a check saw. This answers "where does this
// version sit", and a job that resolved its own versions needs an order for
// them or its cursor could never advance past them — a `steps run` against an
// unpolled resource would repeat its whole fan-out every time.
func (s *Store) VersionOrders(ctx context.Context, resourceName string) (map[string]int64, error) {
	type ordered struct {
		encoded string
		order   int64
	}

	rows, err := collect(ctx, s.db, "the version order of "+resourceName,
		`SELECT version_json, check_order FROM resource_versions WHERE pipeline_id = ? AND resource_name = ?`,
		[]any{s.pipelineID, resourceName}, func(rows *sql.Rows) (ordered, error) {
			var row ordered

			return row, rows.Scan(&row.encoded, &row.order)
		})
	if err != nil {
		return nil, err
	}

	orders := make(map[string]int64, len(rows))
	for _, row := range rows {
		orders[row.encoded] = row.order
	}

	return orders, nil
}

// RecordVersionOrder files a version if it is not already known and returns
// the order it sits at, so a caller that resolved its own versions can
// advance a cursor over them.
func (s *Store) RecordVersionOrder(ctx context.Context, resourceName, versionJSON string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	defer func() { _ = tx.Rollback() }()

	err = ensureVersion(ctx, tx, s.pipelineID, resourceName, versionJSON)
	if err != nil {
		return 0, err
	}

	var order int64

	err = tx.QueryRowContext(ctx,
		`SELECT check_order FROM resource_versions
		 WHERE pipeline_id = ? AND resource_name = ? AND version_json = ?`,
		s.pipelineID, resourceName, versionJSON).Scan(&order)
	if err != nil {
		return 0, fmt.Errorf("could not read version order for %q: %w", resourceName, err)
	}

	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	return order, nil
}

// ensureVersion records a version as seen, so a row that references it has a
// parent to point at.
//
// The foreign keys make this necessary rather than tidy: a job may go green
// on, or fan out over, a version no poll ever recorded — every `steps run`
// resolves its own versions by running the check, and nothing about that
// path goes near the poller. Recording that a job used a version implies the
// version existed, so the implication is made explicit here rather than
// left to fail as a constraint violation.
func ensureVersion(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName, versionJSON string) error {
	next, err := nextCheckOrder(ctx, tx, pipelineID, resourceName)
	if err != nil {
		return err
	}

	// from_check stays 0: this records that a version was USED, which is not
	// the same as a check reporting what exists. See ResourceVersionsJSON.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO resource_versions (pipeline_id, resource_name, version_json, check_order, from_check)
		VALUES (?, ?, ?, ?, 0)
		ON CONFLICT (pipeline_id, resource_name, version_json) DO NOTHING
	`, pipelineID, resourceName, versionJSON, next)
	if err != nil {
		return fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	return nil
}

// GreenVersions returns the versions of a resource that EVERY named upstream
// job has gone green against, oldest first by discovery order.
//
// This is what a passed:-constrained get chooses among. The constraint is
// enforced at RESOLUTION rather than only at trigger time, because a gate
// checked at enqueue and forgotten by the build is checked against a world
// that can change in between: a poll validates v5, a newer v6 lands before a
// worker claims the job, and a build resolving plain "latest" ships the
// version nothing tested. It is also what makes `steps run` honor passed: at
// all — a manual run resolves through the same path a triggered one does.
//
// Considered regardless of from_check: a version's green record proves a
// build fetched it, which is better evidence of existence than a check
// listing. Concourse's model, and the reason a job whose head keeps failing
// upstream still deploys the newest version that DID pass.
func (s *Store) GreenVersions(ctx context.Context, resourceName string, upstreamJobs []string) ([]map[string]any, error) {
	// Every version for which no named upstream lacks a green record: relational division, answered off job_versions' primary key.
	encoded, err := collect(ctx, s.db, "green versions of "+resourceName, `
		SELECT rv.version_json FROM resource_versions rv
		WHERE rv.pipeline_id = ? AND rv.resource_name = ?
		  AND NOT EXISTS (
		      SELECT 1 FROM json_each(?) upstream
		      WHERE NOT EXISTS (
		          SELECT 1 FROM job_versions jv
		          WHERE jv.pipeline_id = rv.pipeline_id AND jv.job_name = upstream.value
		            AND jv.resource_name = rv.resource_name AND jv.version_json = rv.version_json
		      )
		  )
		ORDER BY rv.check_order
	`, []any{s.pipelineID, resourceName, jsonList(upstreamJobs)}, scanString)
	if err != nil {
		return nil, err
	}

	versions := make([]map[string]any, 0, len(encoded))

	for _, one := range encoded {
		version, err := store.DecodeVersion(one)
		if err != nil {
			return nil, fmt.Errorf("could not read green versions for %q: %w", resourceName, err)
		}

		versions = append(versions, version)
	}

	return versions, nil
}
