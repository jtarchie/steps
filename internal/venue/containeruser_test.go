package venue

// Which user a container on a WORKER runs as.

import (
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

func intPtr(v int) *int { return &v }

// TestContainerUserIsDecidedFromTheWorker is the seam. The default exists to
// stop a container writing files into a bind-mounted tree that the machine
// holding that tree cannot then read or delete — and for a placed step that
// machine is the worker, so the identity and the platform are both its own.
//
// Computing them here answers about a different computer: a Linux
// orchestrator against a root shim produced --user 1000:1000 over a
// root-owned 0700 workdir, which cannot even be read. Both existing tests hid
// it — local: has a matching uid, and the AWS one runs from darwin where the
// default is empty either way.
func TestContainerUserIsDecidedFromTheWorker(t *testing.T) {
	t.Parallel()

	for name, test := range map[string]struct {
		goos     string
		uid, gid *int
		spec     string
		want     string
	}{
		"a root linux worker gets root":       {goos: "linux", uid: intPtr(0), gid: intPtr(0), want: "0:0"},
		"an unprivileged linux worker":        {goos: "linux", uid: intPtr(1000), gid: intPtr(1000), want: "1000:1000"},
		"a darwin worker defers to the image": {goos: "darwin", uid: intPtr(501), gid: intPtr(20), want: ""},
		"a shim that did not say":             {goos: "linux", want: ""},
		"an explicit user crosses verbatim":   {goos: "linux", uid: intPtr(0), gid: intPtr(0), spec: "app", want: "app"},
	} {
		session := &session{
			goos: test.goos, uid: test.uid, gid: test.gid,
			container: shell.RunnerSpec{Image: "alpine", User: test.spec},
		}

		got := session.containerSpec("unix:///tmp/x.sock").User
		if got != test.want {
			t.Errorf("%s: user = %q, want %q", name, got, test.want)
		}
	}
}

// TestContainerSpecMountsTheWorkersTree pins the other worker-derived answer
// in the same place, since a bind mount is resolved by the daemon holding it.
func TestContainerSpecMountsTheWorkersTree(t *testing.T) {
	t.Parallel()

	session := &session{
		workdir:   "/var/tmp/steps/steps-shim/abc/work",
		container: shell.RunnerSpec{Image: "alpine", Cwd: "/this/machine/only"},
	}

	spec := session.containerSpec("unix:///tmp/x.sock")

	if spec.MountPath != "/var/tmp/steps/steps-shim/abc/work" {
		t.Errorf("mount = %q, want the tree the shim reported", spec.MountPath)
	}

	if spec.Worker != "" {
		t.Errorf("worker = %q, want it cleared — this runner is a plain container", spec.Worker)
	}

	// The spine of the whole feature, and the one line neither half's tests
	// crossed: the socket is proven to forward and the runner is proven to
	// honour DockerHost, but nothing carried the resolved socket ACROSS. Drop
	// it and every placed image: step runs on the ORCHESTRATOR's daemon,
	// against a mount path that exists only on the worker — and a local:
	// worker cannot tell, because both ends are the same daemon there.
	if spec.DockerHost != "unix:///tmp/x.sock" {
		t.Errorf("docker host = %q, want the worker's forwarded socket", spec.DockerHost)
	}
}

// TestContainerSpecMountsNothingWithoutATree: a resource check: has no
// directory, and a container for it on the worker mounts nothing — as it
// does locally — rather than the shim's empty scratch.
func TestContainerSpecMountsNothingWithoutATree(t *testing.T) {
	t.Parallel()

	session := &session{
		workdir:   "/var/tmp/steps/steps-shim/abc/work",
		container: shell.RunnerSpec{Image: "alpine"},
	}

	if got := session.containerSpec("unix:///tmp/x.sock").MountPath; got != "" {
		t.Errorf("MountPath = %q, want none for a command with no tree", got)
	}
}
