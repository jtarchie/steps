package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// TestRepairChallenges pins the contract: a header that already parses is
// left byte-for-byte alone, one that only parses after commas are inserted
// is rewritten, and one that is broken some other way is left alone so the
// SDK's error still describes what the server actually sent.
func TestRepairChallenges(t *testing.T) {
	t.Parallel()

	const metabase = `Bearer realm="mcp" resource_metadata="https://backerkit-metabase.metabaseapp.com/.well-known/oauth-protected-resource/api/metabase-mcp"`

	tests := []struct {
		name  string
		given []string
		want  []string
	}{
		{
			name:  "compliant header is untouched",
			given: []string{`Bearer realm="mcp", resource_metadata="https://example.com/meta"`},
			want:  []string{`Bearer realm="mcp", resource_metadata="https://example.com/meta"`},
		},
		{
			name:  "bare scheme is untouched",
			given: []string{"Bearer"},
			want:  []string{"Bearer"},
		},
		{
			name:  "space-separated params gain commas",
			given: []string{metabase},
			want: []string{
				`Bearer realm="mcp", resource_metadata="https://backerkit-metabase.metabaseapp.com/.well-known/oauth-protected-resource/api/metabase-mcp"`,
			},
		},
		{
			name:  "unquoted values gain commas too",
			given: []string{`Bearer error=invalid_token error_description="the token expired"`},
			want:  []string{`Bearer error=invalid_token, error_description="the token expired"`},
		},
		{
			name:  "space-separated challenges are separated too",
			given: []string{`Bearer realm="mcp" Basic realm="legacy"`},
			want:  []string{`Bearer realm="mcp", Basic realm="legacy"`},
		},
		{
			name:  "each header line is repaired",
			given: []string{metabase, `Bearer realm="other" error=invalid_token`},
			want: []string{
				`Bearer realm="mcp", resource_metadata="https://backerkit-metabase.metabaseapp.com/.well-known/oauth-protected-resource/api/metabase-mcp"`,
				`Bearer realm="other", error=invalid_token`,
			},
		},
		{
			name:  "unterminated quote is left for the SDK to report",
			given: []string{`Bearer realm="mcp`},
			want:  []string{`Bearer realm="mcp`},
		},
		{
			// The parse oracle cannot catch this one: `Negotiate, YIIK…`
			// parses perfectly well as two challenges whose second scheme is
			// the base64 blob. Two bare tokens in a row is a token68
			// credential, so the value is left alone instead.
			name:  "token68 credential is not split into a second challenge",
			given: []string{"Negotiate YIIKlwYGKwYBBQUCoIIK"},
			want:  []string{"Negotiate YIIKlwYGKwYBBQUCoIIK"},
		},
		{
			name:  "a repairable line is repaired even beside an unrepairable one",
			given: []string{metabase, `Bearer realm="oops`},
			want: []string{
				`Bearer realm="mcp", resource_metadata="https://backerkit-metabase.metabaseapp.com/.well-known/oauth-protected-resource/api/metabase-mcp"`,
				`Bearer realm="oops`,
			},
		},
		{
			name:  "a whitespace-only value is not emptied",
			given: []string{"   "},
			want:  []string{"   "},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			header := http.Header{}
			for _, value := range test.given {
				header.Add("WWW-Authenticate", value)
			}

			repairChallenges(header)

			got := header.Values("WWW-Authenticate")
			if len(got) != len(test.want) {
				t.Fatalf("got %d values %q, want %d", len(got), got, len(test.want))
			}

			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("value %d =\n  %q\nwant\n  %q", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestRepairChallengesYieldsParsedParams is the point of the exercise: the
// resource_metadata URL the login flow needs survives the rewrite.
func TestRepairChallengesYieldsParsedParams(t *testing.T) {
	t.Parallel()

	header := http.Header{}
	header.Set("WWW-Authenticate", `Bearer realm="mcp" resource_metadata="https://example.com/.well-known/oauth-protected-resource/api/mcp"`)

	repairChallenges(header)

	challenges, err := oauthex.ParseWWWAuthenticate(header.Values("WWW-Authenticate"))
	if err != nil {
		t.Fatalf("ParseWWWAuthenticate after repair: %v", err)
	}

	if len(challenges) != 1 {
		t.Fatalf("got %d challenges, want 1", len(challenges))
	}

	if got := challenges[0].Params["resource_metadata"]; got != "https://example.com/.well-known/oauth-protected-resource/api/mcp" {
		t.Errorf("resource_metadata = %q", got)
	}
}

// TestChallengeRepairClientRepairsInFlight checks the wiring, not just the
// string surgery: a response fetched through the login flow's client comes
// back with a parseable header.
func TestChallengeRepairClientRepairsInFlight(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="mcp" resource_metadata="https://example.com/meta"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	resp, err := challengeRepairClient().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	_, err = oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		t.Fatalf("ParseWWWAuthenticate on the response: %v", err)
	}
}

// TestRepairChallengesDropsAnOversizedValue guards the bound on what reaches
// the SDK's challenge parser.
//
// oauthex.splitChallenges rescans the header remainder per unresolvable
// comma, so its cost grows with the square of the input, and Go accepts a
// 10MB response header by default. This repair layer is the only thing
// between the response and that parser, so it is the only place the size can
// be judged before the cost is paid.
func TestRepairChallengesDropsAnOversizedValue(t *testing.T) {
	t.Parallel()

	oversized := "Bearer " + strings.Repeat(",", maxChallengeBytes)

	header := http.Header{}
	header.Add("WWW-Authenticate", oversized)
	header.Add("WWW-Authenticate", `Bearer realm="mcp" resource_metadata="https://example.test/prm"`)

	start := time.Now()

	repairChallenges(header)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("repairChallenges took %s on an oversized header", elapsed)
	}

	got := header.Values("WWW-Authenticate")
	if len(got) != 1 {
		t.Fatalf("WWW-Authenticate = %q, want the oversized value dropped and the real one kept", got)
	}

	// The survivor is the legitimate challenge, repaired as usual — dropping
	// one value must not cost the login the other value would have completed.
	if !strings.Contains(got[0], "resource_metadata=") {
		t.Fatalf("surviving challenge = %q, want the repairable one to be kept", got[0])
	}

	parsed, err := oauthex.ParseWWWAuthenticate(got)
	if err != nil {
		t.Fatalf("ParseWWWAuthenticate(%q): %v", got, err)
	}

	if len(parsed) == 0 {
		t.Fatalf("ParseWWWAuthenticate(%q) returned no challenges", got)
	}
}
