package sqlite

// run_placements: where a placed step ran, and what reaching that machine
// cost in bytes.

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// placementColumns is what RecordPlacement writes and RunPlacements reads
// back, in the field order of Placement.
const placementColumns = `run_id, step_index, step_name, job_name, slot, node_hash,
	tag, address, instance_id,
	goos, goarch, workdir, fstype, fs_free, uid, gid,
	image, bytes_sent`

// RecordPlacement stores where one placed step ran.
//
// REPLACES on conflict, unlike agent usage, which accumulates. Tokens are
// spend and every attempt was paid for; a placement is a description of the
// machine, and a step that ran twice against the same node ran on whichever
// machine answered last. Summing "which filesystem" has no meaning.
func (s *Store) RecordPlacement(ctx context.Context, placement store.Placement) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_placements (`+placementColumns+`, pipeline_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(pipeline_id, run_id, slot) DO UPDATE SET
			step_index = excluded.step_index,
			node_hash  = excluded.node_hash,
			step_name  = excluded.step_name,
			job_name   = excluded.job_name,
			tag         = excluded.tag,
			address     = excluded.address,
			instance_id = excluded.instance_id,
			goos    = excluded.goos,
			goarch  = excluded.goarch,
			workdir = excluded.workdir,
			fstype  = excluded.fstype,
			fs_free = excluded.fs_free,
			uid = excluded.uid,
			gid = excluded.gid,
			image      = excluded.image,
			bytes_sent = excluded.bytes_sent,
			created_at = excluded.created_at`,
		placement.RunID, placement.StepIndex, placement.StepName, placement.JobName,
		placement.Slot, nullableHash(placement.NodeHash),
		placement.Tag, placement.Address, placement.InstanceID,
		placement.GOOS, placement.GOARCH, placement.Workdir, placement.FSType, placement.FSFree,
		placement.UID, placement.GID,
		placement.Image, placement.BytesSent,
		s.pipelineID, now())
	if err != nil {
		return fmt.Errorf("recording a placement: %w", err)
	}

	return nil
}

// RunPlacements returns where every placed step of one run ran, in plan order.
func (s *Store) RunPlacements(ctx context.Context, runID string) ([]store.Placement, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+placementColumns+`
		FROM run_placements
		WHERE run_id = ? AND pipeline_id = ?
		ORDER BY step_index`, runID, s.pipelineID)
	if err != nil {
		return nil, fmt.Errorf("reading placements: %w", err)
	}

	defer func() { _ = rows.Close() }()

	return collectPlacements(rows)
}

func collectPlacements(rows *sql.Rows) ([]store.Placement, error) {
	var placements []store.Placement

	for rows.Next() {
		var (
			placement store.Placement
			nodeHash  sql.NullString
		)

		err := rows.Scan(
			&placement.RunID, &placement.StepIndex, &placement.StepName, &placement.JobName,
			&placement.Slot, &nodeHash,
			&placement.Tag, &placement.Address, &placement.InstanceID,
			&placement.GOOS, &placement.GOARCH, &placement.Workdir, &placement.FSType, &placement.FSFree,
			&placement.UID, &placement.GID,
			&placement.Image, &placement.BytesSent)
		if err != nil {
			return nil, fmt.Errorf("reading a placement: %w", err)
		}

		placement.NodeHash = nodeHash.String

		placements = append(placements, placement)
	}

	err := rows.Err()
	if err != nil {
		return nil, fmt.Errorf("reading placements: %w", err)
	}

	return placements, nil
}
