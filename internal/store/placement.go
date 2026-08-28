package store

// run_placements: where a placed step ran, and what reaching that machine
// cost in bytes.

import (
	"context"
	"database/sql"
	"fmt"
)

// Placement is one placed step's record of the machine that ran it.
//
// InstanceID, UID and GID are pointers because absent and zero are different
// answers. Only an aws:// worker has an instance at all; only a shim that can
// answer reports an identity, and uid 0 is ROOT — the common case under the
// aws:// bootstrap — so an int could not tell "ran as root" from "did not
// say", and those mean opposite things to a reader.
type Placement struct {
	RunID      string
	StepIndex  int
	StepName   string
	JobName    string
	NodeHash   string
	Tag        string
	Address    string
	InstanceID *string
	GOOS       string
	GOARCH     string
	Workdir    string
	FSType     string
	FSFree     int64
	UID        *int
	GID        *int
	Image      string
	BytesSent  int64
}

// placementColumns is what RecordPlacement writes and RunPlacements reads
// back, in the field order of Placement.
const placementColumns = `run_id, step_index, step_name, job_name, node_hash,
	tag, address, instance_id,
	goos, goarch, workdir, fstype, fs_free, uid, gid,
	image, bytes_sent`

// RecordPlacement stores where one placed step ran.
//
// REPLACES on conflict, unlike agent usage, which accumulates. Tokens are
// spend and every attempt was paid for; a placement is a description of the
// machine, and a step that ran twice against the same node ran on whichever
// machine answered last. Summing "which filesystem" has no meaning.
func (s *Store) RecordPlacement(ctx context.Context, placement Placement) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_placements (`+placementColumns+`, pipeline_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, node_hash) DO UPDATE SET
			step_index = excluded.step_index,
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
		placement.RunID, placement.StepIndex, placement.StepName, placement.JobName, placement.NodeHash,
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
func (s *Store) RunPlacements(ctx context.Context, runID string) ([]Placement, error) {
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

func collectPlacements(rows *sql.Rows) ([]Placement, error) {
	var placements []Placement

	for rows.Next() {
		var placement Placement

		err := rows.Scan(
			&placement.RunID, &placement.StepIndex, &placement.StepName, &placement.JobName, &placement.NodeHash,
			&placement.Tag, &placement.Address, &placement.InstanceID,
			&placement.GOOS, &placement.GOARCH, &placement.Workdir, &placement.FSType, &placement.FSFree,
			&placement.UID, &placement.GID,
			&placement.Image, &placement.BytesSent)
		if err != nil {
			return nil, fmt.Errorf("reading a placement: %w", err)
		}

		placements = append(placements, placement)
	}

	err := rows.Err()
	if err != nil {
		return nil, fmt.Errorf("reading placements: %w", err)
	}

	return placements, nil
}
