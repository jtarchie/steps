package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestLoopbackCallbackRejectsUnrecognizedRequests covers the two ways the
// loopback listener used to be a denial-of-service on the login it exists to
// complete. The port is on 127.0.0.1, so ANY local process — a stale tab, a
// prefetch, a port scan, a page the user has open — can reach it.
//
// Before: handle pushed every request it saw into a size-1 channel with no
// state check, so a bare GET /callback was consumed as the answer AND filled
// the buffer; the genuine redirect that followed then blocked forever on the
// send, hanging the browser tab and leaking the handler goroutine, while the
// flow aborted on a state mismatch it could never recover from.
func TestLoopbackCallbackRejectsUnrecognizedRequests(t *testing.T) {
	t.Parallel()

	cb, err := newLoopbackCallback(context.Background(), 0)
	if err != nil {
		t.Fatalf("newLoopbackCallback: %v", err)
	}

	defer cb.Close()

	cb.expect("the-real-state")

	// A stray request with no state at all, and one carrying somebody else's
	// state, are both refused rather than banked.
	for _, query := range []string{"", "?code=x&state=wrong"} {
		if status := getStatus(t, cb.redirectURL+query); status != http.StatusBadRequest {
			t.Errorf("request %q status = %d, want %d", query, status, http.StatusBadRequest)
		}
	}

	// The real redirect still gets through, and its iss is carried rather
	// than dropped — an RFC 9207 server rejects the exchange without it.
	status := getStatus(t, cb.redirectURL+"?code=real-code&state=the-real-state&iss=https://issuer.example")
	if status != http.StatusOK {
		t.Errorf("real redirect status = %d, want %d", status, http.StatusOK)
	}

	select {
	case res := <-cb.result:
		if res.code != "real-code" {
			t.Errorf("code = %q, want the real redirect's", res.code)
		}

		if res.iss != "https://issuer.example" {
			t.Errorf("iss = %q, want it carried through to the SDK", res.iss)
		}
	default:
		t.Fatal("the real redirect delivered nothing")
	}
}

// TestLoopbackCallbackDoesNotBlockOnASecondRedirect pins the other half: once
// a result is banked, a further request is answered rather than parked on a
// full channel forever.
func TestLoopbackCallbackDoesNotBlockOnASecondRedirect(t *testing.T) {
	t.Parallel()

	cb, err := newLoopbackCallback(context.Background(), 0)
	if err != nil {
		t.Fatalf("newLoopbackCallback: %v", err)
	}

	defer cb.Close()

	cb.expect("s")

	for range 2 {
		// The second call is the one that used to deadlock. http.Get has no
		// timeout of its own, so a hang here fails the test by timing out.
		getStatus(t, cb.redirectURL+"?code=c&state=s")
	}
}

type lineSink chan string

func (s lineSink) Write(p []byte) (int, error) {
	s <- string(p)

	return len(p), nil
}

// A browser that would not open leaves the printed URL as the only way forward, and the person has to be told so.
func TestPrintAndOpenSaysWhenTheBrowserDidNotOpen(t *testing.T) {
	t.Parallel()

	out := make(lineSink, 4)

	PrintAndOpen(out, func(string) error { return errors.New("no display") }, "https://as.test/authorize")

	if got := <-out; !strings.Contains(got, "https://as.test/authorize") {
		t.Fatalf("first line = %q, want the URL", got)
	}

	select {
	case got := <-out:
		if !strings.Contains(got, "could not open a browser") || !strings.Contains(got, "no display") {
			t.Fatalf("second line = %q, want the failure named", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing said the browser did not open")
	}
}

// getStatus performs a GET against a test-only loopback URL and returns its
// status, closing the body.
func getStatus(t *testing.T, rawURL string) int {
	t.Helper()

	resp, err := http.Get(rawURL) //nolint:gosec,noctx // test-only, loopback URL built by the caller
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}

	defer func() { _ = resp.Body.Close() }()

	return resp.StatusCode
}

// issBrowserOpen is fakeBrowserOpen for an authorization server that returns
// RFC 9207's iss on the redirect — which some do whether or not their
// metadata says so.
