package mcp

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// challengeRepairClient returns the *http.Client the login flow's transport
// uses: http.DefaultTransport plus the WWW-Authenticate repair below. Only
// the login path needs it — that is the only place a 401's challenge is
// parsed (the SDK's OAuthHandler.Authorize); run/watch attach an already
// persisted token and never read the header.
func challengeRepairClient() *http.Client {
	return &http.Client{Transport: &wwwAuthenticateRepair{base: http.DefaultTransport}}
}

// wwwAuthenticateRepair rewrites a response's WWW-Authenticate header into
// the strictly RFC 9110-compliant form the SDK's parser demands, for the
// real servers that emit auth-params separated by spaces instead of commas
// — e.g. Metabase answers with
//
//	Bearer realm="mcp" resource_metadata="https://…/.well-known/oauth-protected-resource/api/metabase-mcp"
//
// which oauthex.ParseWWWAuthenticate rejects ("expected comma after
// value"), aborting the whole login before the browser ever opens. The
// challenge is unambiguous either way, so refusing to log in over a missing
// comma helps nobody.
type wwwAuthenticateRepair struct {
	base http.RoundTripper
}

func (rt *wwwAuthenticateRepair) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err //nolint:wrapcheck // a RoundTripper middleware must pass its base's error through untouched
	}

	repairChallenges(resp.Header)

	return resp, nil
}

// maxChallengeBytes bounds one WWW-Authenticate value, above which it is
// dropped rather than parsed.
//
// The SDK's splitChallenges (oauthex/resource_meta.go) rescans the remainder
// of the header for every comma it cannot resolve, which is quadratic: a
// value that is mostly commas costs ~200ms at 200k of them and grows with the
// square. Go allows a 10MB response header by default, so a server that wants
// to can hand `steps mcp login` hours of CPU in a single 401.
//
// Bounding it here is not tidiness — this repair layer is the only thing
// between the response and that parser, so it is the only place the size can
// be judged before the cost is paid. 64KB is orders of magnitude past any
// real challenge (they run to a few hundred bytes), so nothing legitimate is
// near it, and a value above it is not a challenge worth trying to honour.
// Dropping rather than truncating: half a challenge parses into something
// that was never sent, and discovery has a well-known-URI path to fall back
// to when a challenge is absent.
const maxChallengeBytes = 64 << 10

// repairChallenges replaces header's WWW-Authenticate values with
// comma-separated ones, but only where that changes a value and the result
// parses — using the SDK's own parser as the oracle, so a header broken in
// some other way (an unterminated quote, say) reaches the caller verbatim
// and the error it produces describes what the server actually sent.
//
// The oracle is applied per VALUE, not to the list: a server that sends two
// WWW-Authenticate lines, one repairable and one broken beyond it, must
// still get the repairable one repaired — otherwise one junk line vetoes the
// login the other line's resource_metadata would have completed.
//
// Note the gate is "the rewrite changes something", not "the original fails
// to parse": the SDK's parser also *silently mis-parses* a space-separated
// challenge whose first value is unquoted, swallowing every later param
// into it (`error=invalid_token resource_metadata="…"` yields one param
// worth of garbage, no error). A compliant header survives the rewrite
// byte-for-byte apart from separator whitespace, so there is nothing to
// protect by skipping it.
func repairChallenges(header http.Header) {
	key := http.CanonicalHeaderKey("WWW-Authenticate")

	values := header.Values(key)
	if len(values) == 0 {
		return
	}

	repaired := make([]string, 0, len(values))
	changed := false

	for _, value := range values {
		if len(value) > maxChallengeBytes {
			slog.Warn("mcp.challenge.oversized",
				"bytes", len(value), "limit", maxChallengeBytes,
				"detail", "dropping a WWW-Authenticate value too large to be a real challenge")

			changed = true

			continue
		}

		fixed := repairChallenge(value)
		changed = changed || fixed != value

		repaired = append(repaired, fixed)
	}

	if !changed {
		return
	}

	header[key] = repaired
}

// repairChallenge returns value with RFC 9110 separators, or value itself
// when the rewrite changes nothing, empties a non-empty header, or produces
// something the SDK's parser still rejects.
func repairChallenge(value string) string {
	rewritten := commaSeparateAuthParams(value)
	if rewritten == value || rewritten == "" {
		return value
	}

	_, err := oauthex.ParseWWWAuthenticate([]string{rewritten})
	if err != nil {
		return value
	}

	return rewritten
}

// itemKind distinguishes the two things a challenge list is made of, which
// is all commaSeparateAuthParams needs to pick the separator between them:
// one space after an auth-scheme, ", " everywhere else.
type itemKind int

const (
	itemNone itemKind = iota
	itemScheme
	itemParam
)

// commaSeparateAuthParams re-serializes one WWW-Authenticate header value
// with RFC 9110 separators, treating a bare token as the auth-scheme that
// starts a new challenge and a token followed by "=" as an auth-param.
// Whitespace and commas between items are both accepted as separators on
// the way in. Anything it cannot make sense of is returned unchanged.
//
// "Cannot make sense of" includes two bare tokens in a row, because that is
// the one shape where the parse oracle in repairChallenge would ratify a
// WRONG guess. `Negotiate YIIKlwYGKwYBBQUCoIIK` is a scheme and a token68
// credential, not two challenges; comma-separating it yields
// `Negotiate, YIIKlwYGKwYBBQUCoIIK`, which parses cleanly as two challenges
// whose second scheme is the base64 blob. Only a scheme followed by an
// auth-param — the space-separated shape this exists for — is rewritten.
func commaSeparateAuthParams(header string) string {
	var (
		out  strings.Builder
		prev itemKind
		pos  int
	)

	for pos < len(header) {
		pos = skipSeparators(header, pos)
		if pos >= len(header) {
			break
		}

		token, next := readToken(header, pos)
		if token == "" {
			return header
		}

		pos = next

		if pos >= len(header) || header[pos] != '=' {
			if prev == itemScheme {
				// A token68 credential, not a second challenge — see above.
				return header
			}

			writeItem(&out, prev, itemScheme, token)

			prev = itemScheme

			continue
		}

		value, next, ok := readValue(header, pos+1)
		if !ok {
			return header
		}

		pos = next

		writeItem(&out, prev, itemParam, token+"="+value)

		prev = itemParam
	}

	return out.String()
}

// writeItem appends text to out with the separator RFC 9110 wants between
// it and whatever preceded it.
func writeItem(out *strings.Builder, prev, kind itemKind, text string) {
	switch {
	case prev == itemNone:
	case prev == itemScheme && kind == itemParam:
		out.WriteString(" ")
	default:
		out.WriteString(", ")
	}

	out.WriteString(text)
}

func skipSeparators(header string, pos int) int {
	for pos < len(header) && (header[pos] == ' ' || header[pos] == '\t' || header[pos] == ',') {
		pos++
	}

	return pos
}

// readToken reads an auth-scheme or auth-param name: everything up to the
// next separator or "=".
func readToken(header string, pos int) (string, int) {
	start := pos
	for pos < len(header) && !strings.ContainsRune(" \t,=", rune(header[pos])) {
		pos++
	}

	return header[start:pos], pos
}

// readValue reads an auth-param value — a quoted string (returned with its
// quotes and escapes intact) or a bare token — reporting false for an
// unterminated quote or an empty value, either of which means this header
// is not something to rewrite.
func readValue(header string, pos int) (string, int, bool) {
	if pos < len(header) && header[pos] == '"' {
		return readQuoted(header, pos)
	}

	start := pos
	for pos < len(header) && header[pos] != ' ' && header[pos] != '\t' && header[pos] != ',' {
		pos++
	}

	if pos == start {
		return "", pos, false
	}

	return header[start:pos], pos, true
}

func readQuoted(header string, pos int) (string, int, bool) {
	start := pos

	for pos++; pos < len(header); {
		switch {
		case header[pos] == '\\' && pos+1 < len(header):
			pos += 2
		case header[pos] == '"':
			return header[start : pos+1], pos + 1, true
		default:
			pos++
		}
	}

	return "", pos, false
}
