package postgres

import (
	"testing"
	"time"
)

// TestConnConfigPinsWhatEveryConnectionNeeds: a bound on connecting, since
// pgx would otherwise wait on a blackholed host forever; UTC, since readers
// compare timestamps against it; a search_path of this driver's schema
// alone, whatever the url asked to search; and a name for pg_stat_activity,
// unless the url chose one.
func TestConnConfigPinsWhatEveryConnectionNeeds(t *testing.T) {
	t.Parallel()

	for rawURL, want := range map[string]struct {
		timeout time.Duration
		schema  string
		app     string
	}{
		"postgres://u@h/db":                                   {10 * time.Second, DefaultSchema, "steps"},
		"postgres://u@h/db?connect_timeout=3":                 {3 * time.Second, DefaultSchema, "steps"},
		"postgres://u@h/db?search_path=ci,public":             {10 * time.Second, "ci", "steps"},
		`postgres://u@h/db?search_path=%22Mixed%22%22Case%22`: {10 * time.Second, `Mixed"Case`, "steps"},
		"postgres://u@h/db?application_name=ci-east":          {10 * time.Second, DefaultSchema, "ci-east"},
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

		if config.RuntimeParams["application_name"] != want.app {
			t.Errorf("%s: application_name = %q, want %q — pg_stat_activity should name steps", rawURL, config.RuntimeParams["application_name"], want.app)
		}
	}
}
