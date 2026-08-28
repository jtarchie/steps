package shim

// The store URL is a credential, and where a transfer is allowed to go is
// part of what it authorizes.

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func redirectTo(t *testing.T, from, to string) error {
	t.Helper()

	origin, err := http.NewRequestWithContext(t.Context(), http.MethodPut, from, nil)
	if err != nil {
		t.Fatal(err)
	}

	next, err := http.NewRequestWithContext(t.Context(), http.MethodPut, to, nil)
	if err != nil {
		t.Fatal(err)
	}

	return sameHostOnly(next, []*http.Request{origin})
}

// TestStoreRedirectsStayOnTheirOwnOrigin covers both halves of the origin.
//
// The host half is the exfiltration this guard was written for. The scheme
// half is the same exposure reached more cheaply: an on-path answer cannot
// move a presigned URL to another host, but downgrading it to http replays
// the body and the signature in the clear without ever leaving the host.
func TestStoreRedirectsStayOnTheirOwnOrigin(t *testing.T) {
	signed := "https://bucket.s3.amazonaws.com/wire/abc?X-Amz-Signature=deadbeef"

	for _, target := range []string{
		"https://attacker.example/wire/abc?X-Amz-Signature=deadbeef",
		"http://bucket.s3.amazonaws.com/wire/abc?X-Amz-Signature=deadbeef",
	} {
		err := redirectTo(t, signed, target)
		if !errors.Is(err, errOffsiteRedirect) {
			t.Errorf("redirect to %q was allowed (%v), want a refusal", target, err)
		}
	}

	err := redirectTo(t, signed, "https://bucket.s3.amazonaws.com/wire/abc?X-Amz-Signature=other")
	if err != nil {
		t.Errorf("a same-origin redirect was refused: %v", err)
	}
}

// TestStoreErrorsDoNotCarryTheSignature is the redaction the shim's failures
// need because they become a FrameError the venue prints, streams to the web
// UI and writes to the state database.
func TestStoreErrorsDoNotCarryTheSignature(t *testing.T) {
	signed := "https://bucket.s3.amazonaws.com/wire/abc?X-Amz-Signature=deadbeef&X-Amz-Credential=AKIA"

	err := storeError("fetching an artifact", &url.Error{
		Op:  "Get",
		URL: signed,
		Err: errors.New("connection refused"), //nolint:err113 // a stand-in for a transport failure
	})

	message := err.Error()
	for _, secret := range []string{"X-Amz-Signature", "deadbeef", "AKIA"} {
		if strings.Contains(message, secret) {
			t.Errorf("a store failure carried %q: %s", secret, message)
		}
	}

	// Still says which object, or the message names nothing an operator can act on.
	if !strings.Contains(message, "bucket.s3.amazonaws.com/wire/abc") {
		t.Errorf("a store failure lost the object it was about: %s", message)
	}
}
