package exprlang

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPEnvelopeKeepsIntegerDigits: a check builds versions out of
// response bodies, and a version is an identity — an id wider than 2^53
// through float64 becomes a number the API never issued, which is then
// stored, hashed, and handed back as the next check's cursor. The shell and
// mcp backends were fixed for exactly this; the envelope is the expr
// backend's copy. Integers come through as int64 (exact); fractions stay
// float64 so arithmetic works, and a fractional IDENTITY belongs in a
// string, as Slack's own ts already is.
func TestHTTPEnvelopeKeepsIntegerDigits(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"id":1234567890123456789,"ts":1699887654.001200}]}`))
	}))
	t.Cleanup(server.Close)

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  http({url: %q}).json.items | map((
	    {id: #.id, ts: #.ts}
	  ))
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if id := fmt.Sprint(versions[0]["id"]); id != "1234567890123456789" {
		t.Errorf("id = %s, want the digits the API sent", id)
	}
}

// TestHTTPEnvelopeNumbersStayComputable: the exactness above must not cost
// arithmetic — a json.Number left in the tree would break `count + 1`, which
// is why numbers normalize to int64/float64 rather than staying strings.
func TestHTTPEnvelopeNumbersStayComputable(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count": 3, "page": 1.5}`))
	}))
	t.Cleanup(server.Close)

	versions, err := RunCheck(context.Background(), fmt.Sprintf(`
	  let r = http({url: %q}).json;
	  [{n: string(r.count + 1), half: string(r.page * 2)}]
	`, server.URL), Input{})
	if err != nil {
		t.Fatalf("RunCheck: %v", err)
	}

	if versions[0]["n"] != "4" {
		t.Errorf("count+1 = %v, want 4", versions[0]["n"])
	}
}
