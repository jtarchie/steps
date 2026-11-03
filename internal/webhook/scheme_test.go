package webhook

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAPlainSchemeDoesNotParseTheBodyBeforeVerifying: a scheme with no expressions signs the raw body, so a forged request must not buy a JSON decode of up to max_body. Alloc count is the observable: decoding this body costs thousands of allocations, a bare HMAC check a handful.
func TestAPlainSchemeDoesNotParseTheBodyBeforeVerifying(t *testing.T) {
	body := []byte("[" + strings.Repeat(`{"a":"b"},`, 5000) + `{}]`)
	req := Request{Method: http.MethodPost, Header: http.Header{"X-Sig": {"forged"}}, Body: body}
	scheme := Scheme{Header: "X-Sig"}

	if scheme.Verify(req, []byte("secret"), time.Now()) {
		t.Fatal("forged signature verified")
	}

	if allocs := testing.AllocsPerRun(10, func() { scheme.Verify(req, []byte("secret"), time.Now()) }); allocs > 50 {
		t.Errorf("Verify made %.0f allocations on a forged request, want a bare HMAC check (the body was parsed first)", allocs)
	}
}
