package venue

import (
	"errors"
	"os/exec"
	"regexp"
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
		{name: "local with root", raw: "local:/srv/steps", scheme: SchemeLocal, root: "/srv/steps"},
		{name: "local with a relative root", raw: "local:srv/steps", wantErr: true},
		{name: "docker+ssh", raw: "docker+ssh://jt@box:2222", scheme: SchemeDockerSSH, user: "jt", host: "box:2222"},
		{name: "docker+ssh naming a disk", raw: "docker+ssh://box/mnt/fast", wantErr: true},
		{name: "docker+ssh without host", raw: "docker+ssh://", wantErr: true},
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

func TestParseWorkerDockerSocket(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"docker+ssh://box": defaultDockerSocket,
		"docker+ssh://box?sock=/run/user/1000/docker.sock": "/run/user/1000/docker.sock",
	} {
		worker, err := ParseWorker(raw)
		if err != nil || worker.Socket != want {
			t.Errorf("ParseWorker(%q).Socket = %q, %v; want %q", raw, worker.Socket, err, want)
		}
	}

	_, err := ParseWorker("ssh://box?sock=/s")
	if err == nil {
		t.Error("ssh:// accepted ?sock=, which only a docker+ worker reads")
	}
}

// TestParseWorkerOptions pins the query options, which are how an operator
// says "this key", "these host keys", "that binary".
func TestParseWorkerOptions(t *testing.T) {
	t.Parallel()

	worker, err := ParseWorker("ssh://jt@box?identity=/k&known_hosts=/kh")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	if worker.Identity != "/k" || worker.KnownHosts != "/kh" {
		t.Errorf("options = %+v, want both carried", worker)
	}

	_, err = ParseWorker("ssh://box?binary=/b")
	if err == nil {
		t.Error("ssh:// accepted ?binary=, which nothing pushes any more")
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

// TestParseWorkerRefusesOptionsItDoesNotKnow is typo protection with money
// attached: ?capactiy=od silently launching spot, or ?identity= silently
// ignored on a scheme that cannot use it, is a mapping that LOOKS configured
// and is not.
func TestParseWorkerRefusesOptionsItDoesNotKnow(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"aws://launch/lt-0def4567890abcde?capactiy=od", // the typo that pays on-demand
		"ssh://box?identiy=/home/jt/.ssh/id",
		"aws://i-0abc123def456789?identity=/home/jt/.ssh/id", // right key, wrong scheme
		"ssh://box?region=us-west-2",
		"aws://i-0abc123def456789?idle=5m",
		"local:?shim=/usr/local/bin/steps",
	} {
		_, err := ParseWorker(raw)
		if !errors.Is(err, ErrWorker) {
			t.Errorf("ParseWorker(%q) = %v, want ErrWorker", raw, err)
		}
	}
}

// TestPlacementCheckRefusesBeforeMoneyIsSpent pins that a dial certain to
// fail is refused with what the invocation already knows — not after an
// acquisition rung launches a billed instance to discover it.
func TestPlacementCheckRefusesBeforeMoneyIsSpent(t *testing.T) {
	t.Parallel()

	bare, err := ParseWorker("aws://launch/lt-0def4567890abcde")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	err = bare.PlacementCheck(true)
	if err == nil {
		t.Error("a worker with no way to get a shim passed the placement check")
	}

	pushed, err := ParseWorker("aws://i-0abc123def456789?binary=/tmp/steps-linux-amd64")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	err = pushed.PlacementCheck(false)
	if err == nil {
		t.Error("?binary= without an artifact store passed the placement check")
	}

	err = pushed.PlacementCheck(true)
	if err != nil {
		t.Errorf("a fully specified worker was refused: %v", err)
	}

	baked, err := ParseWorker("aws://i-0abc123def456789?shim=/usr/local/bin/steps")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	err = baked.PlacementCheck(false)
	if err != nil {
		t.Errorf("an AMI-baked worker needs no store and was refused: %v", err)
	}
}

// Address is what the run record and the browser show of an ssh worker: the user is part of which machine it was, and a mapping without one gains no stray @.
func TestSSHAddressKeepsTheUserOnlyWhenOneWasWritten(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string]string{
		"ssh://jt@box/scratch": "ssh://jt@box/scratch",
		"ssh://box":            "ssh://box",
	} {
		worker, err := ParseWorker(raw)
		if err != nil {
			t.Fatalf("ParseWorker(%q): %v", raw, err)
		}

		if got := worker.Address(); got != want {
			t.Errorf("Address of %q = %q, want %q", raw, got, want)
		}
	}
}

// TestLaunchLabelNamesTheMachineNotTheSpelling pins that steps-worker follows
// registryKey — every spelling of one machine carries one label, and anything
// that makes a different machine a different one — and that nothing of the
// URL, which can carry ?hostkey= and ?binary= paths, reaches a label.
func TestLaunchLabelNamesTheMachineNotTheSpelling(t *testing.T) {
	t.Parallel()

	label := func(raw string) string {
		t.Helper()

		worker, err := ParseWorker(raw)
		if err != nil {
			t.Fatalf("ParseWorker(%q): %v", raw, err)
		}

		labels := worker.launchLabels()
		// The host label is this machine's name, which a runner may legitimately spell "tmpl-…".
		delete(labels, labelHost)

		for _, value := range labels {
			if strings.Contains(value, "lt-0def") || strings.Contains(value, "tmpl") || strings.Contains(value, "secret") {
				t.Errorf("label value %q carries part of %q", value, raw)
			}
		}

		return worker.launchLabels()[labelWorker]
	}

	assertDistinct := func(base string, others ...string) {
		t.Helper()

		for _, different := range others {
			if got := label(different); got == base {
				t.Errorf("%q labels %q, the same as a different machine", different, got)
			}
		}
	}

	awsBase := label("aws://launch/lt-0def4567890abcde?version=1&capacity=spot&region=us-east-1")
	for _, same := range []string{
		"aws://launch/lt-0def4567890abcde/var/tmp?capacity=spot&version=1&region=us-east-1",
		"aws://launch/lt-0def4567890abcde?version=1&capacity=spot&region=us-east-1&idle=5m",
		"aws://launch/lt-0def4567890abcde?version=1&capacity=spot&region=us-east-1&binary=/secret/steps",
	} {
		if got := label(same); got != awsBase {
			t.Errorf("%q labels %q, want the same machine as %q", same, got, awsBase)
		}
	}

	assertDistinct(awsBase,
		"aws://launch/lt-0def4567890abcdf?version=1&capacity=spot&region=us-east-1",
		"aws://launch/lt-0def4567890abcde?version=2&capacity=spot&region=us-east-1",
		"aws://launch/lt-0def4567890abcde?version=1&capacity=od&region=us-east-1",
		"aws://launch/lt-0def4567890abcde?version=1&capacity=spot&region=us-west-2",
	)

	assertDistinct(label("gcp://launch/tmpl?project=p&zone=z"),
		"gcp://launch/tmpl?project=q&zone=z",
		"gcp://launch/tmpl?project=p&zone=y",
	)
}

func TestLabelValue(t *testing.T) {
	t.Parallel()

	gceValue := regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)

	for _, tc := range []struct{ in, want string }{
		{"Foo.Local", "foo-local"},
		{strings.Repeat("a", 100), strings.Repeat("a", 63)},
		{"", "unknown"},
		{"héllo", "h-llo"},
		{"box_1-a", "box_1-a"},
	} {
		got := labelValue(tc.in)
		if got != tc.want {
			t.Errorf("labelValue(%q) = %q, want %q", tc.in, got, tc.want)
		}

		if !gceValue.MatchString(got) {
			t.Errorf("labelValue(%q) = %q, which GCE refuses", tc.in, got)
		}
	}
}
