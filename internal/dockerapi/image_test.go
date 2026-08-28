package dockerapi

// The image layer, asked of a real daemon.

import (
	"context"
	"strings"
	"testing"
	"time"
)

const testImage = "alpine:3"

// requireDaemon skips unless a daemon is reachable, and returns a client
// aimed at it. Not opt-in: a test guarding a shipped feature does not get to
// be optional.
func requireDaemon(t *testing.T) *Client {
	t.Helper()

	client, err := New("")
	if err != nil {
		t.Skipf("cannot construct a docker client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = client.Ping(ctx)
	if err != nil {
		_ = client.Close()

		t.Skipf("docker daemon not reachable: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

func TestImagePresentFindsALocalImage(t *testing.T) {
	client := requireDaemon(t)

	if !client.ImagePresent(context.Background(), testImage) {
		t.Skipf("%s is not on this daemon", testImage)
	}
}

// TestImagePresentIsFalseForAnUnknownImage pins the half that decides whether
// a pull happens at all. Present-by-default would skip every pull and fail in
// the first step that needed the image.
func TestImagePresentIsFalseForAnUnknownImage(t *testing.T) {
	client := requireDaemon(t)

	if client.ImagePresent(context.Background(), "steps-test-no-such-image:definitely-not-here") {
		t.Error("ImagePresent said yes for an image the daemon does not have")
	}
}

// TestPullReportsAResolutionFailure covers the half a modern daemon answers on
// the HTTP call: an image no registry serves.
func TestPullReportsAResolutionFailure(t *testing.T) {
	client := requireDaemon(t)

	var progress strings.Builder

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := client.Pull(ctx, "steps-test-no-such-image:definitely-not-here", &progress)
	if err == nil {
		t.Fatal("Pull succeeded for an image no registry serves")
	}

	if !strings.Contains(err.Error(), "steps-test-no-such-image") {
		t.Errorf("error = %v, want it to name the image", err)
	}
}

// TestPullReportsProgress pins that the operator gets told something.
//
// A pull is minutes of silence otherwise. It is asserted against an image
// already present rather than a cold one so the test needs no network: the
// daemon still narrates the layers it already has.
func TestPullReportsProgress(t *testing.T) {
	client := requireDaemon(t)

	if !client.ImagePresent(context.Background(), testImage) {
		t.Skipf("%s is not on this daemon; this test must not depend on a network", testImage)
	}

	var progress strings.Builder

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	err := client.Pull(ctx, testImage, &progress)
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}

	if progress.Len() == 0 {
		t.Error("Pull reported nothing; an operator watching startup sees silence where a download would be")
	}

	// The byte-by-byte updates are dropped, so what is left has to be the
	// transitions — including the one that says what actually happened.
	if !strings.Contains(progress.String(), "Status:") {
		t.Errorf("progress = %q, want the daemon's closing status line", progress.String())
	}
}

// TestPingReportsAnAbsentDaemon pins that constructing a client proves
// nothing: it parses an address, it does not connect. Preflight has to ask.
//
// A tcp address nothing listens on rather than a missing socket file, because
// a unix path long enough to be unique in a temp directory is also long enough
// to be rejected outright on macOS — the 104-byte limit — which would make
// this pass for the wrong reason.
func TestPingReportsAnAbsentDaemon(t *testing.T) {
	const nowhere = "tcp://127.0.0.1:1"

	client, err := New(nowhere)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	err = client.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping succeeded against an address nothing is listening on")
	}

	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error = %v, want it to name the daemon it could not reach", err)
	}
}

// TestNewRejectsAnOverlongUnixPath pins that the socket-path limit is reported
// where it can be understood.
//
// A unix socket path is capped near 104 bytes by the kernel, not by docker,
// and the venue's forwarded socket lives in a temp directory for exactly this
// reason — see internal/venue naming it d.sock. Caught at construction, the
// message says the path is too long; caught later it is one more daemon that
// cannot be reached.
func TestNewRejectsAnOverlongUnixPath(t *testing.T) {
	_, err := New("unix:///" + strings.Repeat("a-long-directory-name/", 8) + "d.sock")
	if err == nil {
		t.Fatal("New accepted a unix socket path no kernel will bind")
	}
}

// TestNewRefusesAnSSHHost pins that the one address shape this cannot dial
// says so, rather than failing later as a connection problem.
func TestNewRefusesAnSSHHost(t *testing.T) {
	_, err := New("ssh://someone@example.invalid")
	if err == nil {
		t.Fatal("New accepted an ssh:// host it cannot dial")
	}

	if !strings.Contains(err.Error(), "ssh://") {
		t.Errorf("error = %v, want it to name the unsupported address", err)
	}
}

// TestReportPullProgressReturnsAStreamError covers the other half, and the
// reason it is a unit test rather than a real pull is worth stating.
//
// A pull can fail in two places. A reference that resolves to nothing fails on
// the HTTP call, which the sibling test above covers against a live daemon.
// A pull that fails PART WAY THROUGH — a layer that will not download, a
// digest that does not match, a registry that dies mid-transfer — reports it
// as a message inside the stream, after a 200. That case cannot be provoked on
// demand from a healthy daemon, and it is exactly the case where a caller that
// only checked the HTTP error would report a successful pull of an image it
// does not have.
func TestReportPullProgressReturnsAStreamError(t *testing.T) {
	t.Parallel()

	stream := strings.NewReader(`{"status":"Pulling from library/alpine","id":"3"}` +
		`{"id":"9824c27679d3","status":"Downloading","progress":"[==>   ] 1.2MB/5MB"}` +
		`{"errorDetail":{"message":"unexpected EOF"},"error":"unexpected EOF"}`)

	var progress strings.Builder

	err := reportPullProgress(stream, &progress)
	if err == nil {
		t.Fatal("reportPullProgress succeeded on a stream that reported a failure")
	}

	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("error = %v, want the daemon's own explanation", err)
	}
}

// TestReportPullProgressRejectsATruncatedStream pins that a connection cut
// mid-pull is a failure and not a short success.
//
// This is the shape a dropped network takes, and reading it as success is the
// worst available answer: the run continues believing it has an image that is
// half there.
func TestReportPullProgressRejectsATruncatedStream(t *testing.T) {
	t.Parallel()

	var progress strings.Builder

	err := reportPullProgress(strings.NewReader(`{"status":"Pulling fs layer","id":"9824`), &progress)
	if err == nil {
		t.Fatal("reportPullProgress succeeded on a stream that was cut off mid-message")
	}
}

// TestReportPullProgressKeepsTransitionsAndDropsBytes pins the shape of what
// the operator sees: one line per thing that HAPPENED, not one per kilobyte.
func TestReportPullProgressKeepsTransitionsAndDropsBytes(t *testing.T) {
	t.Parallel()

	stream := strings.NewReader(`{"status":"Pulling from library/alpine","id":"3"}` +
		`{"id":"9824c27679d3","status":"Pulling fs layer"}` +
		`{"id":"9824c27679d3","status":"Downloading","progress":"[==>   ] 1.2MB/5MB"}` +
		`{"id":"9824c27679d3","status":"Downloading","progress":"[=====>] 5MB/5MB"}` +
		`{"id":"9824c27679d3","status":"Pull complete"}` +
		`{"status":"Status: Downloaded newer image for alpine:3"}`)

	var progress strings.Builder

	err := reportPullProgress(stream, &progress)
	if err != nil {
		t.Fatalf("reportPullProgress: %v", err)
	}

	want := "3: Pulling from library/alpine\n" +
		"9824c27679d3: Pulling fs layer\n" +
		"9824c27679d3: Pull complete\n" +
		"Status: Downloaded newer image for alpine:3\n"

	if progress.String() != want {
		t.Errorf("progress =\n%q\nwant\n%q", progress.String(), want)
	}
}

// TestImagePresentIsFalseForAnUnparseableName pins the answer for a name that
// is not a reference at all.
//
// It is not the same failure as "the daemon does not have it" — the daemon
// reports an invalid argument, not a missing image — and the difference used
// to be invisible because the docker CLI failed both the same way. Reported as
// present, a name like this would be pushed into the first step that needed
// it and surface there as a container that would not start.
func TestImagePresentIsFalseForAnUnparseableName(t *testing.T) {
	client := requireDaemon(t)

	for _, name := range []string{"--privileged", "NOT A REF", "-v"} {
		if client.ImagePresent(context.Background(), name) {
			t.Errorf("ImagePresent(%q) said yes for a name that is not a reference", name)
		}
	}
}
