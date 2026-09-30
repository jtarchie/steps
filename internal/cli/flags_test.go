package cli

import (
	"strings"
	"testing"
)

func TestDBAcceptsWhatADriverCanOpen(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"state.db",
		"sqlite://state.db",
		"postgres://ci@db.internal/steps",
		"postgresql://ci@db.internal:5433/steps?sslmode=verify-full&search_path=ci",
		// A unix socket names no host in the authority, and libpq reads it from the query.
		"postgres:///steps?host=/var/run/postgresql",
	} {
		var db DB

		err := db.UnmarshalText([]byte(raw))
		if err != nil {
			t.Errorf("UnmarshalText(%q) = %v, want it accepted", raw, err)
		}
	}
}

func TestDBRefusesWhatWouldOpenTheWrongThing(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"mysql://u:hunter22@h/db":      "no driver for mysql://",
		"postgres:steps":               "postgres: needs //",
		"postgresql:steps":             "postgres: needs //",
		"sqlite:state.db":              "sqlite: needs //",
		"sqlite://":                    "names no file",
		"state.db?_busy_timeout=1":     "no query string",
		"postgres://u:hunter22@h:x/db": "does not parse",
	} {
		var db DB

		err := db.UnmarshalText([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("UnmarshalText(%q) = %v, want a refusal saying %q", raw, err, want)

			continue
		}

		if strings.Contains(err.Error(), "hunter22") {
			t.Errorf("refusing %q printed its password: %v", raw, err)
		}
	}
}

// A State is printed in the daemon's banner, its errors and every hint; String is what %s calls, so it is the redaction.
func TestStatePrintsWithoutItsPassword(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{ //nolint:gosec // G101: fixture passwords, the thing being redacted
		"postgres://ci:hunter22@db/steps":                          "postgres://ci@db/steps",
		"postgres://ci@db/steps?password=hunter22&sslmode=require": "postgres://ci@db/steps?sslmode=require",
		"postgres://ci@db/steps?sslpassword=hunter22":              "postgres://ci@db/steps",
		".steps/app.yml.db":                                        ".steps/app.yml.db",
	} {
		if got := State(raw).String(); got != want {
			t.Errorf("State(%q).String() = %q, want %q", raw, got, want)
		}
	}
}

func TestShellArgQuotesOnlyWhatAShellWouldSplit(t *testing.T) {
	t.Parallel()

	for value, want := range map[string]string{
		".steps/app.yml.db":       ".steps/app.yml.db",
		"postgres://ci@db/steps":  "postgres://ci@db/steps",
		"postgres://db/s?a=1&b=2": "'postgres://db/s?a=1&b=2'",
		"it's here.db":            `'it'\''s here.db'`,
		"":                        "''",
	} {
		if got := shellArg(value); got != want {
			t.Errorf("shellArg(%q) = %s, want %s", value, got, want)
		}
	}
}
