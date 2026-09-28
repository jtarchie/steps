package cli

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// FuzzDBUnmarshalText holds --db to what the driver can open: no query string for it to swallow the pragmas with, no scheme but sqlite, and never a value that silently reads as the flag not given.
func FuzzDBUnmarshalText(f *testing.F) {
	for _, seed := range []string{"", "state.db", "sqlite://state.db", "sqlite://", "sqlite:state.db", "postgres://u:p@h/db", "a.db?_busy_timeout=1", "://"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		var db DB

		err := db.UnmarshalText([]byte(raw))
		if err != nil {
			checkRefusalHidesURL(t, raw, err)

			return
		}

		scheme, _, isURL := strings.Cut(raw, "://")
		if string(db) != raw || strings.Contains(raw, "?") || (isURL && scheme != "sqlite") {
			t.Fatalf("UnmarshalText(%q) accepted %q", raw, db)
		}

		if raw != "" && db.path() == "" {
			t.Fatalf("UnmarshalText(%q) names no file, which reads as the default database", raw)
		}
	})
}

// checkRefusalHidesURL pins that an unknown driver is refused by scheme alone: past it, a network url carries credentials, and a usage error lands in shell history.
func checkRefusalHidesURL(t *testing.T, raw string, err error) {
	t.Helper()

	scheme, rest, isURL := strings.Cut(raw, "://")
	if !isURL || scheme == "sqlite" || !strings.Contains(rest, "@") || strings.Contains(scheme, rest) {
		return
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
