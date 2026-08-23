package venue

import (
	"os/exec"
	"strings"
	"testing"
)

// TestParseWorkerForms pins the small grammar. It is small on purpose:
// anything describing the MACHINE rather than the connection belongs to
// whatever provisioned it, not to a pipeline runner dialing in.
func TestParseWorkerForms(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		raw     string
		scheme  Scheme
		user    string
		host    string
		root    string
		wantErr bool
	}{
		{name: "local", raw: "local:", scheme: SchemeLocal},
		{name: "ssh with user", raw: "ssh://jt@box", scheme: SchemeSSH, user: "jt", host: "box"},
		{name: "ssh with port", raw: "ssh://box:2222", scheme: SchemeSSH, host: "box:2222"},
		{name: "ssh with root", raw: "ssh://jt@box/srv/steps", scheme: SchemeSSH, user: "jt", host: "box", root: "/srv/steps"},
		{name: "ssh without host", raw: "ssh://", wantErr: true},
		{name: "local naming a host", raw: "local://box", wantErr: true},
		{name: "unknown scheme", raw: "http://box", wantErr: true},
		{name: "empty", raw: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			worker, err := ParseWorker(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseWorker(%q) succeeded, want a refusal", tc.raw)
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseWorker(%q): %v", tc.raw, err)
			}

			if worker.Scheme != tc.scheme || worker.User != tc.user || worker.Host != tc.host || worker.Root != tc.root {
				t.Errorf("ParseWorker(%q) = %+v, want scheme %q user %q host %q root %q",
					tc.raw, worker, tc.scheme, tc.user, tc.host, tc.root)
			}
		})
	}
}

// TestParseWorkerOptions pins the query options, which are how an operator
// says "this key", "these host keys", "that binary".
func TestParseWorkerOptions(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("ssh://jt@box?identity=/k&known_hosts=/kh&binary=/b")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Identity != "/k" || worker.KnownHosts != "/kh" || worker.Binary != "/b" {
		t.Errorf("options = %+v, want all three carried", worker)
	}
}

// TestWorkerRootStaysAbsolute pins the difference between naming a disk and
// naming a directory in somebody's home.
//
// The path was stripped of its leading slash, so ssh://box/mnt/fast asked for
// a relative "mnt/fast" — which the worker resolves against the login user's
// home. An operator mapping a machine's fast disk got their home directory
// instead: nothing on the disk they named, and the root filesystem filling up
// with build trees.
func TestWorkerRootStaysAbsolute(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("ssh://jt@box/mnt/fast")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Root != "/mnt/fast" {
		t.Fatalf("root = %q, want %q — a relative root resolves against $HOME on the worker", worker.Root, "/mnt/fast")
	}

	if got := remoteShimPath(worker, "abc123"); !strings.HasPrefix(got, "/mnt/fast/") {
		t.Errorf("remote shim path = %q, want it under the named root", got)
	}
}

// TestShellQuoteSurvivesAPathAShellWouldSplit pins that the remote start
// command is one argument.
//
// An SSH exec request is a string the far end hands to a shell, so an unquoted
// path is subject to word splitting: a disk mounted at /mnt/fast disk becomes
// two arguments and the shim never starts, with nothing in the error naming
// the space as the reason.
func TestShellQuoteSurvivesAPathAShellWouldSplit(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"/mnt/fast disk/steps", "/mnt/it's/steps", "/tmp/steps"} {
		quoted := shellQuote(path)

		out, err := exec.CommandContext(t.Context(), "sh", "-c", "printf %s "+quoted).Output() //nolint:gosec // the point of the test
		if err != nil {
			t.Fatalf("running the quoted path through a shell: %v", err)
		}

		if string(out) != path {
			t.Errorf("a shell read %q as %q — the remote start command is not one argument", path, out)
		}
	}
}
