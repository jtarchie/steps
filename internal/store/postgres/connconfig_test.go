package postgres

import (
	"testing"
	"time"
)

// TestConnConfigPinsWhatEveryConnectionNeeds: a bound on connecting, since
// pgx would otherwise wait on a blackholed host forever; UTC, since readers
// compare timestamps against it; and a search_path of this driver's schema
// alone, whatever the url asked to search.
func TestConnConfigPinsWhatEveryConnectionNeeds(t *testing.T) {
	t.Parallel()

	for rawURL, want := range map[string]struct {
		timeout time.Duration
		schema  string
	}{
		"postgres://u@h/db":                                   {10 * time.Second, DefaultSchema},
		"postgres://u@h/db?connect_timeout=3":                 {3 * time.Second, DefaultSchema},
		"postgres://u@h/db?search_path=ci,public":             {10 * time.Second, "ci"},
		`postgres://u@h/db?search_path=%22Mixed%22%22Case%22`: {10 * time.Second, `Mixed"Case`},
	} {
		config, schema, err := connConfig(rawURL)
		if err != nil {
			t.Fatalf("connConfig(%q): %v", rawURL, err)
		}

		if config.ConnectTimeout != want.timeout {
			t.Errorf("%s: ConnectTimeout = %s, want %s", rawURL, config.ConnectTimeout, want.timeout)
		}

		if schema != want.schema || config.RuntimeParams["search_path"] != quote(want.schema) {
			t.Errorf("%s: schema %q, search_path %q; want %q alone", rawURL, schema, config.RuntimeParams["search_path"], want.schema)
		}

		if config.RuntimeParams["TimeZone"] != "UTC" {
			t.Errorf("%s: TimeZone = %q, want UTC", rawURL, config.RuntimeParams["TimeZone"])
		}
	}
}
