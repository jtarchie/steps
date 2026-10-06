package postgres

// webhook_deliveries: each delivery a webhook resource receives, as the
// version it is and the payload a get writes out.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordDelivery files the version, its payload and — unless held — the
// resource's current version and the jobs it triggers, in one transaction.
func (s *Store) RecordDelivery(ctx context.Context, resourceName string, delivery store.Delivery, dispatch store.Dispatch, limit int) (bool, error) {
	encoded, err := store.EncodeVersion(delivery.Version)
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	headers, err := json.Marshal(delivery.Headers)
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	body := delivery.Body
	if body == nil {
		body = []byte{}
	}

	recorded := false

	err = s.write(ctx, func(tx *sql.Tx) error {
		added, err := insertNewVersions(ctx, tx, s.pipelineID, resourceName, []string{encoded})
		if err != nil || added == 0 {
			return err
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO webhook_deliveries (pipeline_id, resource_name, version_json, body, headers_json)
			VALUES ($1, $2, $3, $4, $5)
		`, s.pipelineID, resourceName, encoded, body, string(headers))
		if err != nil {
			return err //nolint:wrapcheck // wrapped below with the resource
		}

		err = s.dispatchDelivery(ctx, tx, resourceName, encoded, dispatch)
		if err != nil {
			return err
		}

		recorded = true

		return pruneAfterDelivery(ctx, tx, s.pipelineID, resourceName, encoded, limit)
	})
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	return recorded, nil
}

func (s *Store) dispatchDelivery(ctx context.Context, tx *sql.Tx, resourceName, encoded string, dispatch store.Dispatch) error {
	if dispatch.Held {
		return nil
	}

	err := recordCheckedVersion(ctx, tx, s.pipelineID, resourceName, encoded)
	if err != nil {
		return err
	}

	for _, job := range dispatch.Jobs {
		err = enqueueJob(ctx, tx, s.pipelineID, job, resourceName)
		if err != nil {
			return err
		}
	}

	return nil
}

// pruneAfterDelivery is RecordVersions' cap, with the delivery as the whole
// report.
func pruneAfterDelivery(ctx context.Context, tx *sql.Tx, pipelineID int64, resourceName, encoded string, limit int) error {
	if limit < 0 {
		limit = store.DefaultResourceVersionCap
	}

	if limit == 0 {
		return nil
	}

	return pruneVersions(ctx, tx, pipelineID, resourceName, limit, []string{encoded})
}

// Delivery reads back the payload recorded with a version.
func (s *Store) Delivery(ctx context.Context, resourceName, versionJSON string) (store.Delivery, bool, error) {
	var (
		body    []byte
		headers string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT body, headers_json FROM webhook_deliveries
		WHERE pipeline_id = $1 AND resource_name = $2 AND version_json = $3
	`, s.pipelineID, resourceName, versionJSON).Scan(&body, &headers)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Delivery{}, false, nil
	}

	if err != nil {
		return store.Delivery{}, false, fmt.Errorf("could not read the delivery to %q: %w", resourceName, err)
	}

	version, err := store.DecodeVersion(versionJSON)
	if err != nil {
		return store.Delivery{}, false, fmt.Errorf("could not read the delivery to %q: %w", resourceName, err)
	}

	delivery := store.Delivery{Version: version, Body: body}

	err = json.Unmarshal([]byte(headers), &delivery.Headers)
	if err != nil {
		return store.Delivery{}, false, fmt.Errorf("could not read the delivery to %q: %w", resourceName, err)
	}

	return delivery, true, nil
}
