package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jtarchie/steps/internal/store"
)

// writeDelivery is a webhook get: the delivery recorded with this version, as body, headers.json and version.json.
func writeDelivery(ctx context.Context, st store.Deliveries, name string, version map[string]any, dir string) error {
	encoded, err := store.EncodeVersion(version)
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	delivery, found, err := st.Delivery(ctx, name, encoded)
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	if !found {
		return fmt.Errorf("get %q: no delivery is recorded for version %s — version_history: may have pruned it", name, encoded)
	}

	headers, err := json.MarshalIndent(delivery.Headers, "", "  ")
	if err != nil {
		return fmt.Errorf("get %q: %w", name, err)
	}

	files := []struct {
		name     string
		contents []byte
	}{{"body", delivery.Body}, {"headers.json", headers}, {"version.json", []byte(encoded)}}

	for _, file := range files {
		err = os.WriteFile(filepath.Join(dir, file.name), file.contents, 0o600)
		if err != nil {
			return fmt.Errorf("get %q: %w", name, err)
		}
	}

	return nil
}
