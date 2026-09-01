package iapdial

// The relay protocol against Google's real relay — the conformance test the
// fake cannot replace, for the reason the package doc gives: the fake was
// written from the same reading of the protocol as the client, so a shared
// misreading passes both sides. The known trap is CONNECT_SUCCESS_SID's
// shape (length-prefixed bytes, not a uint64), which two community clients
// misread without noticing; a real dial is what notices.
//
// Opt-in, same fixture as the venue tests:
//
//	hack/gcp-fixture.sh up   # export what it prints
//	go test ./internal/venue/iapdial -run TestRealGCP -v

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// realGCPTarget is the fixture instance, or a skip. The access token comes
// from gcloud rather than an OAuth dependency: this package's allow-list is
// the websocket and nothing else, and fixture plumbing does not get to
// change that.
func realGCPTarget(t *testing.T) (Target, string) {
	t.Helper()

	target := Target{
		Project:  os.Getenv("STEPS_TEST_GCP_PROJECT"),
		Zone:     os.Getenv("STEPS_TEST_GCP_ZONE"),
		Instance: os.Getenv("STEPS_TEST_GCP_INSTANCE"),
		Port:     22,
	}

	if target.Project == "" || target.Zone == "" || target.Instance == "" {
		t.Skip("no GCP fixture — run hack/gcp-fixture.sh up and export what it prints")
	}

	out, err := exec.CommandContext(t.Context(), "gcloud", "auth", "print-access-token").Output()
	if err != nil {
		t.Fatalf("gcloud auth print-access-token: %v", err)
	}

	return target, strings.TrimSpace(string(out))
}

// TestRealGCPRelayCarriesASession dials the real relay to the fixture's
// sshd and reads the SSH version banner back — one round trip proving the
// connect URL, the auth headers, the subprotocol, the SID frame's real
// shape, and that DATA frames carry the backend's bytes.
func TestRealGCPRelayCarriesASession(t *testing.T) {
	target, token := realGCPTarget(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	channel, err := Open(ctx, ConnectURL(target), token)
	if err != nil {
		t.Fatalf("Open against the real relay: %v", err)
	}

	t.Cleanup(func() { _ = channel.Close() })

	if len(channel.SessionID()) == 0 {
		t.Error("the relay sent no session id — or the SID frame was misread")
	}

	// An SSH server speaks first, so the banner arrives unprompted.
	banner := make([]byte, 64)

	deadline := time.Now().Add(30 * time.Second)
	read := 0

	for read < 8 {
		if time.Now().After(deadline) {
			t.Fatalf("no SSH banner after 30s; got %q", banner[:read])
		}

		n, err := channel.Read(banner[read:])
		if err != nil {
			t.Fatalf("reading the banner: %v (got %q)", err, banner[:read])
		}

		read += n
	}

	if !strings.HasPrefix(string(banner[:read]), "SSH-2.0-") {
		t.Fatalf("banner = %q, want an SSH-2.0 identification string", banner[:read])
	}

	// Write our own identification line: sshd answers protocol from here on,
	// which proves the DATA path carries client bytes too (a key-exchange
	// packet follows the banner if the bytes really landed).
	_, err = channel.Write([]byte("SSH-2.0-steps_iapdial_conformance\r\n"))
	if err != nil {
		t.Fatalf("writing the identification: %v", err)
	}

	more := make([]byte, 256)

	n, err := channel.Read(more)
	if err != nil {
		t.Fatalf("reading the key exchange: %v", err)
	}

	if n == 0 {
		t.Fatal("sshd sent nothing after the identification exchange")
	}
}
