package postgres

import (
	"net/url"
	"strings"
	"testing"
)

const secret = "s3cr3t-not-for-logs"

// TestNothingPrintsThePassword: a --db URL may carry one in three places, and
// what this package returns — a description, an error — ends up in a
// terminal, a log and the web UI.
func TestNothingPrintsThePassword(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	host := mustURL(t, server).Host

	for name, rawURL := range map[string]string{
		"userinfo":    "postgres://postgres:" + secret + "@" + host + "/no_such_database?sslmode=disable",
		"password":    "postgres://postgres@" + host + "/no_such_database?sslmode=disable&password=" + secret,
		"sslpassword": "postgres://postgres@" + host + "/no_such_database?sslmode=disable&sslpassword=" + secret,
		"unparseable": "postgres://postgres:" + secret + "@" + host + ":notaport/db",
		"bad option":  "postgres://postgres:" + secret + "@" + host + "/db?connect_timeout=soon",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for what, text := range map[string]string{
				"Redact":       Redact(rawURL),
				"OpenStore":    failureText(t, func() error { _, err := OpenStore(rawURL, "p"); return err }),
				"OpenReader":   failureText(t, func() error { _, err := OpenReader(rawURL); return err }),
				"OpenExisting": failureText(t, func() error { _, err := OpenExisting(rawURL, "p"); return err }),
			} {
				if strings.Contains(text, secret) {
					t.Errorf("%s printed the password: %s", what, text)
				}
			}
		})
	}
}

func failureText(t *testing.T, fn func() error) string {
	t.Helper()

	err := fn()
	if err == nil {
		return ""
	}

	return err.Error()
}

func TestADescriptionCarriesNoPassword(t *testing.T) {
	t.Parallel()

	rawURL := newDatabase(t)
	parsed := mustURL(t, rawURL)
	password, _ := parsed.User.Password()

	st := openStore(t, rawURL, "p")

	if strings.Contains(st.Description(), password) {
		t.Errorf("Description() = %q carries the password", st.Description())
	}

	if !strings.Contains(st.Description(), "search_path=steps") {
		t.Errorf("Description() = %q does not name its schema", st.Description())
	}
}

func TestAWrongPasswordSaysWhereCredentialsGo(t *testing.T) {
	t.Parallel()
	requirePostgres(t)

	parsed := mustURL(t, server)
	parsed.User = url.UserPassword("postgres", secret)

	_, err := OpenStore(parsed.String(), "p")
	if err == nil || !strings.Contains(err.Error(), "PGPASSWORD") {
		t.Fatalf("OpenStore = %v, want the PGPASSWORD advice", err)
	}

	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal printed the password it refused: %v", err)
	}
}

func TestRedactKeepsEverythingButThePassword(t *testing.T) {
	t.Parallel()

	got := Redact("postgres://ci:" + secret + "@db.internal:5433/steps?sslmode=verify-full&password=" + secret + "&search_path=ci")

	for _, want := range []string{"ci@db.internal:5433", "/steps", "sslmode=verify-full", "search_path=ci"} {
		if !strings.Contains(got, want) {
			t.Errorf("Redact = %q, want it to keep %q", got, want)
		}
	}

	if strings.Contains(got, secret) || strings.Contains(got, "password") {
		t.Errorf("Redact = %q still carries the password", got)
	}

	if !HasPassword("postgres://u:p@h/db") || !HasPassword("postgres://u@h/db?password=p") || HasPassword("postgres://u@h/db") {
		t.Error("HasPassword misread one of its three cases")
	}
}
