package store

// resource_versions: every version steps has seen of a resource, in the order
// it saw them.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

// DefaultResourceVersionCap bounds resource_versions per resource when
// nothing says otherwise.
//
// A count rather than an age, for the same reason consumedVersionCap is one:
// what matters is how far behind a job may fall, not how old a version is. A
// resource nobody has polled in a month has not accumulated anything, while a
// busy one accumulates a row per version forever.
//
// The bound is not free of meaning. A version pruned here takes its
// job_versions row with it (ON DELETE CASCADE), so a `passed:` gate can no
// longer clear for it — correct, since a version out of history cannot be
// built, but it means a cap set below what a slow downstream job needs will
// hold that job back. Override with defaults.version_history: in the
// pipeline, or --version-history on the command line.
const DefaultResourceVersionCap = 1000

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

	if limit <= 0 {
		limit = DefaultResourceVersionCap
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	defer func() { _ = tx.Rollback() }()

	added, err := insertNewVersions(ctx, tx, resourceName, versions)
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
	floor, err := minReportedOrder(ctx, tx, resourceName, versions)
	if err != nil {
		return 0, err
	}

	err = pruneVersions(ctx, tx, resourceName, limit, floor)
	if err != nil {
		return 0, err
	}

	err = tx.Commit()
	if err != nil {
		return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
	}

	return added, nil
}

// insertNewVersions files the versions check-history does not hold, each
// taking the next check_order, and reports how many that was.
//
// The WHERE on the upsert is what keeps a steady-state poll free: a row a
// check already filed matches the conflict but not the WHERE, so nothing is
// written and RowsAffected is 0 — the order neither advances nor gaps. A row
// only a run had filed matches both, taking a fresh order (see
// RecordVersions).
func insertNewVersions(ctx context.Context, tx *sql.Tx, resourceName string, versions []map[string]any) (int, error) {
	next, err := nextCheckOrder(ctx, tx, resourceName)
	if err != nil {
		return 0, err
	}

	added := 0

	for _, version := range versions {
		encoded, err := EncodeVersion(version)
		if err != nil {
			return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
		}

		result, err := tx.ExecContext(ctx, `
			INSERT INTO resource_versions (resource_name, version_json, check_order, first_seen_at, from_check)
			VALUES (?, ?, ?, ?, 1)
			ON CONFLICT (resource_name, version_json)
			DO UPDATE SET from_check = 1, check_order = excluded.check_order, first_seen_at = excluded.first_seen_at
			WHERE resource_versions.from_check = 0
		`, resourceName, encoded, next, now())
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
func minReportedOrder(ctx context.Context, tx *sql.Tx, resourceName string, versions []map[string]any) (int64, error) {
	const chunk = 500

	var floor int64 = 1<<62 - 1

	for start := 0; start < len(versions); start += chunk {
		end := min(start+chunk, len(versions))

		args := make([]any, 0, end-start+1)
		args = append(args, resourceName)

		for _, version := range versions[start:end] {
			encoded, err := EncodeVersion(version)
			if err != nil {
				return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
			}

			args = append(args, encoded)
		}

		var lowest sql.NullInt64

		err := tx.QueryRowContext(ctx,
			`SELECT MIN(check_order) FROM resource_versions WHERE resource_name = ? AND version_json IN (`+
				placeholders(end-start)+`)`, args...).Scan(&lowest)
		if err != nil {
			return 0, fmt.Errorf("could not record versions for %q: %w", resourceName, err)
		}

		if lowest.Valid && lowest.Int64 < floor {
			floor = lowest.Int64
		}
	}

	return floor, nil
}

// nextCheckOrder is the order to give the next newly-seen version.
//
// MAX+1 inside the caller's transaction rather than an AUTOINCREMENT column,
// because the sequence is PER RESOURCE: one global counter would still order
// correctly but leave gaps that make "the 50 oldest of this resource" a
// scan rather than a range. Safe against a concurrent writer because the DSN
// opens transactions IMMEDIATE, so this read already holds the write lock.
func nextCheckOrder(ctx context.Context, tx *sql.Tx, resourceName string) (int64, error) {
	var highest sql.NullInt64

	err := tx.QueryRowContext(ctx,
		`SELECT MAX(check_order) FROM resource_versions WHERE resource_name = ?`, resourceName).Scan(&highest)
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
func pruneVersions(ctx context.Context, tx *sql.Tx, resourceName string, limit int, floor int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM resource_versions
		WHERE resource_name = ? AND check_order < ? AND check_order NOT IN (
			SELECT check_order FROM resource_versions
			WHERE resource_name = ?
			ORDER BY check_order DESC
			LIMIT ?
		)
	`, resourceName, floor, resourceName, limit)
	if err != nil {
		return fmt.Errorf("could not prune versions for %q: %w", resourceName, err)
	}

	return nil
}

// ResourceVersions returns the versions a CHECK has reported for a resource,
// oldest first — the same order and the same contract a check's own output
// has, so a caller can treat it as the check's answer without knowing where
// it came from.
//
// Rows that exist only because something referenced them are excluded, and
// an empty result means "nothing has checked this resource, go and check".
// Returning them would be worse than useless: a `steps run` records the one
// version it took, and treating that lone row as the resource's history
// would hide every other version from the next run.
//
// Decoded with UseNumber, for the reason every other version reader has it:
// a version field goes back out to the API that reported it, and float64
// turns an id into exponent notation.
func (s *Store) ResourceVersions(ctx context.Context, resourceName string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT version_json FROM resource_versions
		WHERE resource_name = ? AND from_check = 1
		ORDER BY check_order
	`, resourceName)
	if err != nil {
		return nil, fmt.Errorf("could not read versions for %q: %w", resourceName, err)
	}

	defer func() { _ = rows.Close() }()

	var versions []map[string]any

	for rows.Next() {
		var encoded string

		err = rows.Scan(&encoded)
		if err != nil {
			return nil, fmt.Errorf("could not read versions for %q: %w", resourceName, err)
		}

		version, err := DecodeVersion(encoded)
		if err != nil {
			return nil, fmt.Errorf("could not read versions for %q: %w", resourceName, err)
		}

		versions = append(versions, version)
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read versions for %q: %w", resourceName, err)
	}

	return versions, nil
}

// VersionOrders maps every recorded version of a resource to its
// check_order, INCLUDING the rows a check did not file.
//
// Deliberately wider than ResourceVersions, which answers "what exists" and
// therefore reports only what a check saw. This answers "where does this
// version sit", and a job that resolved its own versions needs an order for
// them or its cursor could never advance past them — a `steps run` against an
// unpolled resource would repeat its whole fan-out every time.
func (s *Store) VersionOrders(ctx context.Context, resourceName string) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version_json, check_order FROM resource_versions WHERE resource_name = ?`, resourceName)
	if err != nil {
		return nil, fmt.Errorf("could not read version order for %q: %w", resourceName, err)
	}

	defer func() { _ = rows.Close() }()

	orders := map[string]int64{}

	for rows.Next() {
		var (
			encoded string
			order   int64
		)

		err = rows.Scan(&encoded, &order)
		if err != nil {
			return nil, fmt.Errorf("could not read version order for %q: %w", resourceName, err)
		}

		orders[encoded] = order
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read version order for %q: %w", resourceName, err)
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

	err = ensureVersion(ctx, tx, resourceName, versionJSON)
	if err != nil {
		return 0, err
	}

	var order int64

	err = tx.QueryRowContext(ctx,
		`SELECT check_order FROM resource_versions WHERE resource_name = ? AND version_json = ?`,
		resourceName, versionJSON).Scan(&order)
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
func ensureVersion(ctx context.Context, tx *sql.Tx, resourceName, versionJSON string) error {
	var highest sql.NullInt64

	err := tx.QueryRowContext(ctx,
		`SELECT MAX(check_order) FROM resource_versions WHERE resource_name = ?`, resourceName).Scan(&highest)
	if err != nil {
		return fmt.Errorf("could not read version order for %q: %w", resourceName, err)
	}

	// from_check stays 0: this records that a version was USED, which is not
	// the same as a check reporting what exists. See ResourceVersions.
	_, err = tx.ExecContext(ctx, `
		INSERT INTO resource_versions (resource_name, version_json, check_order, first_seen_at, from_check)
		VALUES (?, ?, ?, ?, 0)
		ON CONFLICT (resource_name, version_json) DO NOTHING
	`, resourceName, versionJSON, highest.Int64+1, now())
	if err != nil {
		return fmt.Errorf("could not record version for %q: %w", resourceName, err)
	}

	return nil
}

// EncodeVersion renders a version as the canonical JSON every version table
// keys on. json.Marshal sorts map keys, so the same version always produces
// the same string.
func EncodeVersion(version map[string]any) (string, error) {
	encoded, err := json.Marshal(version)
	if err != nil {
		return "", fmt.Errorf("could not encode version: %w", err)
	}

	return string(encoded), nil
}

// DecodeVersion parses one stored version, keeping numbers as exact digits.
func DecodeVersion(encoded string) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader([]byte(encoded)))
	decoder.UseNumber()

	var version map[string]any

	err := decoder.Decode(&version)
	if err != nil {
		return nil, fmt.Errorf("could not decode version: %w", err)
	}

	return version, nil
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
	orders, err := s.VersionOrders(ctx, resourceName)
	if err != nil {
		return nil, err
	}

	green := make(map[string]bool, len(orders))
	for encoded := range orders {
		green[encoded] = true
	}

	for _, upstream := range upstreamJobs {
		passed, err := s.passedVersionSet(ctx, upstream, resourceName)
		if err != nil {
			return nil, err
		}

		for encoded := range green {
			if !passed[encoded] {
				delete(green, encoded)
			}
		}
	}

	encodedVersions := make([]string, 0, len(green))
	for encoded := range green {
		encodedVersions = append(encodedVersions, encoded)
	}

	sort.Slice(encodedVersions, func(i, j int) bool {
		return orders[encodedVersions[i]] < orders[encodedVersions[j]]
	})

	versions := make([]map[string]any, 0, len(encodedVersions))

	for _, encoded := range encodedVersions {
		version, err := DecodeVersion(encoded)
		if err != nil {
			return nil, fmt.Errorf("could not read green versions for %q: %w", resourceName, err)
		}

		versions = append(versions, version)
	}

	return versions, nil
}

// passedVersionSet is every version_json jobName has recorded green for a
// resource — the raw material GreenVersions intersects.
func (s *Store) passedVersionSet(ctx context.Context, jobName, resourceName string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT version_json FROM job_versions WHERE job_name = ? AND resource_name = ?`,
		jobName, resourceName)
	if err != nil {
		return nil, fmt.Errorf("could not read passed versions for job %q: %w", jobName, err)
	}

	defer func() { _ = rows.Close() }()

	passed := map[string]bool{}

	for rows.Next() {
		var encoded string

		err = rows.Scan(&encoded)
		if err != nil {
			return nil, fmt.Errorf("could not read passed versions for job %q: %w", jobName, err)
		}

		passed[encoded] = true
	}

	err = rows.Err()
	if err != nil {
		return nil, fmt.Errorf("could not read passed versions for job %q: %w", jobName, err)
	}

	return passed, nil
}
