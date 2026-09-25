package sqlite

// webhook_deliveries: each delivery a webhook resource receives, as the version it is and the payload a get writes out.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/store"
)

// RecordDelivery files the version, its payload and — unless held — the resource's current version and the jobs it triggers, in one transaction, which is what lets the handler promise a sender that a 2xx lost nothing.
func (s *Store) RecordDelivery(ctx context.Context, resourceName string, delivery store.Delivery, dispatch store.Dispatch, limit int) (bool, error) {
	encoded, err := store.EncodeVersion(delivery.Version)
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	headers, err := json.Marshal(delivery.Headers)
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	defer func() { _ = tx.Rollback() }()

	added, err := insertNewVersions(ctx, tx, s.pipelineID, resourceName, []string{encoded})
	if err != nil || added == 0 {
		return false, err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (pipeline_id, resource_name, version_json, body, headers_json)
		VALUES (?, ?, ?, ?, ?)
	`, s.pipelineID, resourceName, encoded, delivery.Body, string(headers))
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	err = s.dispatchDelivery(ctx, tx, resourceName, encoded, dispatch)
	if err != nil {
		return false, err
	}

	err = s.pruneAfterDelivery(ctx, tx, resourceName, encoded, limit)
	if err != nil {
		return false, err
	}

	err = tx.Commit()
	if err != nil {
		return false, fmt.Errorf("could not record a delivery to %q: %w", resourceName, err)
	}

	return true, nil
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

// pruneAfterDelivery is RecordVersions' cap, with the delivery as the whole report: everything older than the newest limit goes, payloads with it.
func (s *Store) pruneAfterDelivery(ctx context.Context, tx *sql.Tx, resourceName, encoded string, limit int) error {
	if limit < 0 {
		limit = store.DefaultResourceVersionCap
	}

	if limit == 0 {
		return nil
	}

	floor, err := minReportedOrder(ctx, tx, s.pipelineID, resourceName, []string{encoded})
	if err != nil {
		return err
	}

	return pruneVersions(ctx, tx, s.pipelineID, resourceName, limit, floor)
}

// Delivery reads back the payload recorded with a version.
func (s *Store) Delivery(ctx context.Context, resourceName, versionJSON string) (store.Delivery, bool, error) {
	var (
		body    []byte
		headers string
	)

	err := s.db.QueryRowContext(ctx, `
		SELECT body, headers_json FROM webhook_deliveries
		WHERE pipeline_id = ? AND resource_name = ? AND version_json = ?
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
