package mcp

import (
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func withoutSeparators(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == ',' {
			return -1
		}

		return r
	}, s)
}

// FuzzRepairChallenge: a server's WWW-Authenticate value comes back either verbatim or rewritten into something the SDK parses, the rewrite only moves separators, and repairing twice changes nothing more.
func FuzzRepairChallenge(f *testing.F) {
	f.Add(`Bearer realm="mcp" resource_metadata="https://x.test/.well-known/oauth-protected-resource"`)
	f.Add(`Bearer error=invalid_token resource_metadata="https://x"`)
	f.Add(`Negotiate YIIKlwYGKwYBBQUCoIIK`)
	f.Add(`Bearer realm="unterminated`)
	f.Add(`Basic realm="a\"b", Bearer scope=x`)
	f.Add(`=,, "\`)

	f.Fuzz(func(t *testing.T, value string) {
		rewritten := commaSeparateAuthParams(value)
		if rewritten != value && withoutSeparators(rewritten) != withoutSeparators(value) {
			t.Fatalf("commaSeparateAuthParams(%q) = %q, which changed more than separators", value, rewritten)
		}

		repaired := repairChallenge(value)
		if repaired == value {
			return
		}

		if repaired != rewritten {
			t.Fatalf("repairChallenge(%q) = %q, neither the value nor its rewrite %q", value, repaired, rewritten)
		}

		_, err := oauthex.ParseWWWAuthenticate([]string{repaired})
		if err != nil {
			t.Fatalf("repairChallenge(%q) = %q, which the SDK refuses: %v", value, repaired, err)
		}

		if again := repairChallenge(repaired); again != repaired {
			t.Fatalf("repair is not idempotent: %q → %q → %q", value, repaired, again)
		}
	})
}

// FuzzWithScopes: the authorization URL asks for exactly the pipeline's scopes, plus offline_access when the SDK had asked for it, and every other parameter is left as the SDK built it.
//
//nolint:cyclop // one branch per property the fuzzed input is held to
func FuzzWithScopes(f *testing.F) {
	f.Add("https://as.test/authorize?client_id=c&scope=a+b+offline_access&state=s", "read write")
	f.Add("https://as.test/authorize?scope=everything", "")
	f.Add("://bad", "x")
	f.Add("https://as.test/a?scope=x&scope=offline_access", "offline_access")

	f.Fuzz(func(t *testing.T, authURL, scopeList string) {
		scopes := strings.Fields(scopeList)
		got := withScopes(authURL, scopes)

		before, err := url.Parse(authURL)
		if len(scopes) == 0 || err != nil {
			if got != authURL {
				t.Fatalf("withScopes(%q, %q) = %q, want it unchanged", authURL, scopes, got)
			}

			return
		}

		after, err := url.Parse(got)
		if err != nil {
			t.Fatalf("withScopes(%q) = %q, which does not parse: %v", authURL, got, err)
		}

		want := slices.Clone(scopes)
		if slices.Contains(strings.Fields(before.Query().Get("scope")), "offline_access") && !slices.Contains(want, "offline_access") {
			want = append(want, "offline_access")
		}

		query := after.Query()
		if query.Get("scope") != strings.Join(want, " ") || len(query["scope"]) != 1 {
			t.Fatalf("withScopes(%q, %q) asked for %q, want %q", authURL, scopes, query["scope"], strings.Join(want, " "))
		}

		original := before.Query()
		for key, values := range original {
			if key != "scope" && !slices.Equal(query[key], values) {
				t.Fatalf("withScopes(%q) changed %s from %q to %q", authURL, key, values, query[key])
			}
		}

		if len(query) != len(original) && (len(query) != len(original)+1 || original["scope"] != nil) {
			t.Fatalf("withScopes(%q) = %q added or dropped parameters", authURL, got)
		}

		if after.Host != before.Host || after.Scheme != before.Scheme || after.Path != before.Path {
			t.Fatalf("withScopes(%q) = %q moved the endpoint", authURL, got)
		}
	})
}

// FuzzStateFromAuthURL: the state read back is the one the SDK put in the URL, so a callback carrying any other state is refused.
func FuzzStateFromAuthURL(f *testing.F) {
	f.Add("https://as.test/authorize?client_id=c", "abc-123")
	f.Add("http://127.0.0.1:1/cb", "a&b=c#d")

	f.Fuzz(func(t *testing.T, base, state string) {
		parsed, err := url.Parse(base)
		if err != nil {
			if stateFromAuthURL(base) != "" {
				t.Fatalf("stateFromAuthURL(%q) read a state from an unparsable URL", base)
			}

			return
		}

		query := parsed.Query()
		query.Set("state", state)
		parsed.RawQuery = query.Encode()

		if got := stateFromAuthURL(parsed.String()); got != state {
			t.Fatalf("state %q in %q read back as %q", state, parsed.String(), got)
		}
	})
}
