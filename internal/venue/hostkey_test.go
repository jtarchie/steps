package venue

// Verifying a machine that has never been seen before.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestWorkerHostKeyPinIsDialable is the case known_hosts cannot serve: a
// machine that did not exist a minute ago.
//
// Host keys are always checked, and the answer for a known machine is
// known_hosts — "ssh to it once and you have an entry". A venue acquired on
// demand has no once: it is created, used, and destroyed, and whatever
// attested its key did so out of band. A fingerprint on the mapping is that
// attestation, carried the same way every other per-worker connection detail
// is.
func TestWorkerHostKeyPinIsDialable(t *testing.T) {
	server := newTestSSHD(t)

	mapping := server.URLWithHostKeyPin(t)

	worker, err := ParseWorker(mapping)
	if err != nil {
		t.Fatalf("ParseWorker(%q): %v", mapping, err)
	}

	if worker.HostKey == "" {
		t.Fatal("the mapping carried no pin")
	}

	runner := pinnedRunner(t, mapping, t.TempDir())

	out, err := runner.RunCapture(context.Background(), "echo pinned")
	if err != nil {
		t.Fatalf("running against a pinned worker: %v", err)
	}

	if !strings.Contains(string(out), "pinned") {
		t.Errorf("output = %q, want the command's own", out)
	}
}

// TestWorkerHostKeyPinRefusesAnotherMachine is the whole point of a pin: it
// has to be able to say no. A pin that accepted any key would be
// InsecureIgnoreHostKey with a longer spelling.
func TestWorkerHostKeyPinRefusesAnotherMachine(t *testing.T) {
	server := newTestSSHD(t)

	// A syntactically valid fingerprint of some other machine.
	mapping := server.URLWithPin(t, "SHA256:"+strings.Repeat("A", 43))

	runner := pinnedRunner(t, mapping, t.TempDir())

	err := runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("a worker whose host key does not match its pin ran the command")
	}

	if !strings.Contains(err.Error(), "SHA256:") {
		t.Errorf("error = %v, want it to name the fingerprints, so an operator can see which machine answered", err)
	}
}

// TestWorkerRejectsAMalformedPin refuses at parse time rather than at dial
// time. A typo in a fingerprint that only failed on connection would look
// exactly like the machine having been replaced — which is the alarm this
// feature exists to raise, and a false one teaches operators to ignore it.
func TestWorkerRejectsAMalformedPin(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"ssh://box?hostkey=nonsense",
		"ssh://box?hostkey=SHA256:",
		"ssh://box?hostkey=MD5:aa:bb:cc",
	} {
		_, err := ParseWorker(raw)
		if err == nil {
			t.Errorf("ParseWorker(%q) accepted a fingerprint that can never match", raw)

			continue
		}

		if !errors.Is(err, ErrWorker) {
			t.Errorf("ParseWorker(%q) = %v, want a worker error", raw, err)
		}
	}
}

// TestWorkerPinAndKnownHostsConflict refuses naming both rather than picking
// one. They are two different answers to the same question, and an operator
// who supplied both believes something about which wins.
func TestWorkerPinAndKnownHostsConflict(t *testing.T) {
	t.Parallel()

	pin := "SHA256:" + strings.Repeat("A", 43)

	_, err := ParseWorker("ssh://box?hostkey=" + pin + "&known_hosts=/tmp/kh")
	if err == nil {
		t.Fatal("a mapping naming both a pin and a known_hosts file was accepted")
	}
}

// pinnedRunner builds a runner for a mapping that already names its own
// credentials, pushing this test binary as the shim.
func pinnedRunner(t *testing.T, mapping, cwd string) shell.Runner {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	runner, err := NewRunner(shell.RunnerSpec{Cwd: cwd, Worker: mapping + "&binary=" + self})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}

	t.Cleanup(func() { _ = runner.Close() })

	return runner
}
