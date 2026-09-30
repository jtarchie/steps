package cli

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// FuzzDBUnmarshalText holds --db to what a driver can open: a sqlite path with no query string for it to swallow the pragmas with, or a postgres url; no other scheme; and never a value that silently reads as the flag not given.
func FuzzDBUnmarshalText(f *testing.F) {
	for _, seed := range []string{
		"", "state.db", "sqlite://state.db", "sqlite://", "sqlite:state.db", "postgres://u:p@h/db",
		"postgresql://h/db?sslmode=verify-full&password=p", "postgres:db", "mysql://u:p@h/db", "a.db?_busy_timeout=1", "://",
		"postgres://u:p@h:port/db",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		var db DB

		err := db.UnmarshalText([]byte(raw))
		if err != nil {
			checkRefusalHidesURL(t, raw, err)

			return
		}

		checkAccepted(t, raw, db)
	})
}

// checkAccepted pins what an accepted value may be: itself, a sqlite path with no query string or a postgres url, and never the default.
func checkAccepted(t *testing.T, raw string, db DB) {
	t.Helper()

	scheme, _, isURL := strings.Cut(raw, "://")
	postgresURL := isURL && postgresScheme(scheme)

	if string(db) != raw || (strings.Contains(raw, "?") && !postgresURL) || (isURL && scheme != "sqlite" && !postgresURL) {
		t.Fatalf("UnmarshalText(%q) accepted %q", raw, db)
	}

	if raw != "" && db.location() == "" {
		t.Fatalf("UnmarshalText(%q) names no database, which reads as the default", raw)
	}
}

// checkRefusalHidesURL pins that a refusal never repeats what follows the scheme: past it, a network url carries credentials, and a usage error lands in shell history.
func checkRefusalHidesURL(t *testing.T, raw string, err error) {
	t.Helper()

	scheme, rest, isURL := strings.Cut(raw, "://")
	if !isURL || scheme == "sqlite" || !strings.Contains(rest, "@") || strings.Contains(scheme, rest) {
		return
	}

	// A refusal that does not depend on the password cannot be carrying it:
	// the same url with another password is refused in the same words.
	userinfo, _, _ := strings.Cut(rest, "@")
	if user, password, ok := strings.Cut(userinfo, ":"); ok && password != "" {
		var other DB

		masked := scheme + "://" + user + ":" + strings.Repeat("\x01", len(password)) + strings.TrimPrefix(rest, userinfo)

		otherErr := other.UnmarshalText([]byte(masked))
		if otherErr == nil || otherErr.Error() != err.Error() {
			t.Fatalf("refusing %q depends on its password: %v", raw, err)
		}
	}

	if strings.Contains(err.Error(), rest) {
		t.Fatalf("refusing %q echoed the url past its scheme: %v", raw, err)
	}
}

// FuzzParseRerun pins --rerun: no # is every build, and a build after # is a non-negative number of the run before the FIRST #.
func FuzzParseRerun(f *testing.F) {
	for _, seed := range []string{"run", "run#0", "run#12", "run#-1", "run#x", "a#b#1", "#3", "run#"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, value string) {
		runID, build, err := parseRerun(value)

		wantRun, wantBuild, valid := expectedRerun(value)
		if !valid {
			if err == nil {
				t.Fatalf("parseRerun(%q) accepted build %d", value, build)
			}

			return
		}

		if err != nil || runID != wantRun || build != wantBuild {
			t.Fatalf("parseRerun(%q) = %q, %d, %v; want %q, %d", value, runID, build, err, wantRun, wantBuild)
		}
	})
}

func expectedRerun(value string) (string, int, bool) {
	runID, after, found := strings.Cut(value, "#")
	if !found {
		return runID, -1, true
	}

	build, err := strconv.Atoi(after)

	return runID, build, err == nil && build >= 0
}

// FuzzSplitTargetCredentials pins that credentials come off a parseable target exactly as it held them, and that the address left behind does not carry them.
func FuzzSplitTargetCredentials(f *testing.F) {
	for _, seed := range []string{"http://localhost:8080", "https://u:p@steps.example/", "http://u@h/base/", "http://u:p%40x@h", "not a url", "http://u:p@h/%zz"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, target string) {
		address, username, password := splitTargetCredentials(target)

		parsed, err := url.Parse(strings.TrimSuffix(target, "/"))
		if err != nil || parsed.User == nil {
			if address != strings.TrimSuffix(target, "/") || username != "" || password != "" {
				t.Fatalf("splitTargetCredentials(%q) = %q, %q, %q", target, address, username, password)
			}

			return
		}

		wantPassword, _ := parsed.User.Password()
		if username != parsed.User.Username() || password != wantPassword {
			t.Fatalf("splitTargetCredentials(%q) took off %q:%q", target, username, password)
		}

		checkAddressDropsCredentials(t, target, address, username, password)
	})
}

func checkAddressDropsCredentials(t *testing.T, target, address, username, password string) {
	t.Helper()

	// With no host a path starting // re-reads as an authority, so its own text can look like userinfo; such a target dials nothing.
	back, err := url.Parse(address)
	if err != nil || back.Host == "" {
		return
	}

	leftPassword, _ := back.User.Password()
	if (username != "" && back.User.Username() == username) || (password != "" && leftPassword == password) {
		t.Fatalf("splitTargetCredentials(%q) = %q, which still carries %q", target, address, back.User)
	}
}
