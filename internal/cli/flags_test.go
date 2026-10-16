package cli

import (
	"bytes"
	"log/slog"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

// helpOf is what `steps <command> --help` prints.
func helpOf(t *testing.T, command string) string {
	t.Helper()

	var out bytes.Buffer

	parser, err := kong.New(&CLI{}, kong.Name("steps"), kong.Vars{"version": "test"}, kong.Writers(&out, &out), kong.Exit(func(int) {}))
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}

	_, _ = parser.Parse([]string{command, "--help"})

	return out.String()
}

// TestOnlyTheLocalCommandsOfferDeliver: web refused --deliver while its help listed it, inherited from the flags run and test share with it.
func TestOnlyTheLocalCommandsOfferDeliver(t *testing.T) {
	t.Parallel()

	for _, command := range []string{"run", "test"} {
		if !strings.Contains(helpOf(t, command), "--deliver") {
			t.Errorf("steps %s --help does not offer --deliver", command)
		}
	}

	if help := helpOf(t, "web"); strings.Contains(help, "--deliver") {
		t.Errorf("steps web --help offers --deliver, which a daemon has no use for:\n%s", help)
	}
}

// TestReadOnlyHelpNamesWhatItWithholds: it once named a resume control no page has and left out four that it withholds.
func TestReadOnlyHelpNamesWhatItWithholds(t *testing.T) {
	t.Parallel()

	help := strings.Join(strings.Fields(helpOf(t, "web")), " ")

	for _, want := range []string{"trigger", "retry", "approve", "answer", "release", "pause", "abort", "mcp connect and test", "mcp_exports"} {
		if !strings.Contains(help, want) {
			t.Errorf("--read-only help does not name %q:\n%s", want, help)
		}
	}

	if strings.Contains(help, "resume") {
		t.Errorf("--read-only help names a resume control the web UI does not have:\n%s", help)
	}
}

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

// A handler that renders by kind rather than by Stringer — JSON marshals a string-kinded value as its bytes — must not be what decides whether a password is logged.
func TestStateLogsWithoutItsPassword(t *testing.T) {
	t.Parallel()

	var out strings.Builder

	slog.New(slog.NewJSONHandler(&out, nil)).Info("opened", "db", State("postgres://ci:hunter22@db/steps"))

	if strings.Contains(out.String(), "hunter22") || !strings.Contains(out.String(), `"db":"postgres://ci@db/steps"`) {
		t.Errorf("logged %s", out.String())
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

// TestVersionFallsBackToTheModuleVersion: `go install …@v0.1.0` stamps no ldflags, and printed "dev" while the go command had recorded which version it built.
func TestVersionFallsBackToTheModuleVersion(t *testing.T) {
	t.Parallel()

	installed := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, true
	}

	if got := moduleVersion("dev", installed); got != "v0.1.0" {
		t.Errorf("unstamped go install build = %q, want the module version", got)
	}

	if got := moduleVersion("v0.2.0", installed); got != "v0.2.0" {
		t.Errorf("stamped build = %q, want the stamp to win", got)
	}

	devel := func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, true
	}

	if got := moduleVersion("dev", devel); got != "dev" {
		t.Errorf("go run build = %q, want dev", got)
	}

	if got := moduleVersion("dev", func() (*debug.BuildInfo, bool) { return nil, false }); got != "dev" {
		t.Errorf("no build info = %q, want dev", got)
	}
}
