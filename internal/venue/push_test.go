package venue

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pkg/sftp/v2"
	sshfx "github.com/pkg/sftp/v2/encoding/ssh/filexfer"
	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shim"
)

// withShims installs shims as the embedded set for one test. Never from a
// parallel test: the seam is package state, and a parallel test is released
// only once every sequential one has returned and restored it.
func withShims(t *testing.T, shims map[string][]byte) {
	t.Helper()

	files := fstest.MapFS{}
	for name, binary := range shims {
		files[shimsDir+"/"+name] = &fstest.MapFile{Data: binary, Mode: 0o755}
	}

	previous := embeddedShims
	embeddedShims = files
	embeddedBuilds.Clear()

	t.Cleanup(func() {
		embeddedShims = previous
		embeddedBuilds.Clear()
	})
}

// foreignPlatform is a platform steps ships on that is not this machine's.
func foreignPlatform() (string, string) {
	if runtime.GOOS == "linux" && runtime.GOARCH == "amd64" {
		return "darwin", "arm64"
	}

	return "linux", "amd64"
}

func TestParsePlatform(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		uname, goos, goarch string
		ok                  bool
	}{
		{"Linux x86_64", "linux", "amd64", true},
		{"Linux aarch64", "linux", "arm64", true},
		{"Darwin arm64", "darwin", "arm64", true},
		{"Darwin x86_64", "darwin", "amd64", true},
		{"Linux armv7l", "", "", false},
		{"FreeBSD amd64", "", "", false},
		{"../x arm64", "", "", false},
		{"Linux x86_64 extra", "", "", false},
		{"", "", "", false},
	} {
		goos, goarch, ok := parsePlatform(test.uname)
		if goos != test.goos || goarch != test.goarch || ok != test.ok {
			t.Errorf("parsePlatform(%q) = %s/%s %v, want %s/%s %v", test.uname, goos, goarch, ok, test.goos, test.goarch, test.ok)
		}
	}
}

// resolveFixture is a worker of each kind resolveShim tells apart, with an
// embedded shim for the foreign one and an EMPTY file for this machine's.
type resolveFixture struct {
	foreign, host, unknown workerProbe
	self                   string
	embedded               []byte
}

func newResolveFixture(t *testing.T) resolveFixture {
	t.Helper()

	foreignOS, foreignArch := foreignPlatform()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	fixture := resolveFixture{
		foreign:  workerProbe{goos: foreignOS, goarch: foreignArch, known: true, uid: -1},
		host:     workerProbe{goos: runtime.GOOS, goarch: runtime.GOARCH, known: true, uid: -1},
		unknown:  workerProbe{uid: -1},
		self:     self,
		embedded: []byte("a shim for " + foreignOS),
	}

	withShims(t, map[string][]byte{
		shimName(foreignOS, foreignArch):       fixture.embedded,
		shimName(runtime.GOOS, runtime.GOARCH): {},
	})

	return fixture
}

// TestResolveShimPicksBySource is the push's decision table: ?binary= over
// everything, then the embedded shim for the worker's platform, then this
// process when the worker is this platform or would not say.
func TestResolveShimPicksBySource(t *testing.T) {
	fixture := newResolveFixture(t)

	for _, test := range []struct {
		name   string
		worker Worker
		probe  workerProbe
		kind   shimKind
		file   string
	}{
		{"?binary= over the embedded shim", Worker{Binary: fixture.self}, fixture.foreign, kindBinary, fixture.self},
		{"the embedded shim for a foreign worker", Worker{}, fixture.foreign, kindEmbedded, shimName(fixture.foreign.goos, fixture.foreign.goarch)},
		// The host's embedded file is empty: absent, not pushed as zero bytes.
		{"this process when the host's embed is empty", Worker{}, fixture.host, kindSelf, fixture.self},
		{"this process, unverified, for a worker that would not say", Worker{}, fixture.unknown, kindGuess, fixture.self},
	} {
		source, err := resolveShim(test.worker, test.probe)
		if err != nil || source.kind != test.kind || source.name != test.file {
			t.Errorf("%s: got %d %q, %v; want %d %q", test.name, source.kind, source.name, err, test.kind, test.file)
		}
	}

	source, err := resolveShim(Worker{}, fixture.foreign)
	if err != nil || source.build != shim.BuildOfBytes(fixture.embedded) || source.size != int64(len(fixture.embedded)) {
		t.Errorf("embedded shim = %+v, %v, want it keyed by its own hash and size", source, err)
	}
}

// TestResolveShimRefusesAPlatformItHasNoShimFor pins the refusal and the ways
// out it names, before anything is uploaded.
func TestResolveShimRefusesAPlatformItHasNoShimFor(t *testing.T) {
	fixture := newResolveFixture(t)

	other := workerProbe{goos: fixture.foreign.goos, goarch: "riscv64", known: true, uid: -1}

	_, err := resolveShim(Worker{}, other)
	if !errors.Is(err, errNoShimForPlatform) ||
		!strings.Contains(err.Error(), "?binary=") || !strings.Contains(err.Error(), "?shim=") ||
		!strings.Contains(err.Error(), fixture.foreign.goos+"/"+fixture.foreign.goarch) {
		t.Errorf("platform with no shim = %v, want a refusal naming the embedded set, ?binary= and ?shim=", err)
	}

	// This machine's OS on another arch is as foreign as any other.
	sibling := workerProbe{goos: runtime.GOOS, goarch: "riscv64", known: true, uid: -1}

	_, err = resolveShim(Worker{}, sibling)
	if !errors.Is(err, errNoShimForPlatform) {
		t.Errorf("this OS on another arch = %v, want refused rather than sent this binary", err)
	}

	withShims(t, map[string][]byte{})

	_, err = resolveShim(Worker{}, fixture.foreign)
	if !errors.Is(err, errNoShimForPlatform) || !strings.Contains(err.Error(), "task build") {
		t.Errorf("foreign worker, nothing embedded = %v, want `task build` named", err)
	}
}

// statAnswer is an sftp stat answer, as the worker's sftp-server would send it.
func statAnswer(uid uint32, mode fs.FileMode) fs.FileInfo {
	attrs := sshfx.Attributes{}
	attrs.SetUserGroup(uid, uid)
	attrs.SetPermissions(sshfx.FromGoFileMode(mode))

	return &sshfx.NameEntry{Filename: "steps", Attrs: attrs}
}

// TestCheckPrivateRefusesWhatAnotherLoginCouldHavePlanted covers the uid
// branch, which the in-process sshd cannot reach: it logs in as the user
// running the tests, and cannot chown a file to anybody else.
func TestCheckPrivateRefusesWhatAnotherLoginCouldHavePlanted(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		info fs.FileInfo
		ok   bool
	}{
		{"own 0700 file", statAnswer(1000, 0o700), true},
		{"own 0755 dir", statAnswer(1000, fs.ModeDir|0o755), true},
		{"another uid", statAnswer(1001, 0o700), false},
		{"group-writable", statAnswer(1000, 0o770), false},
		{"world-writable dir", statAnswer(1000, fs.ModeDir|0o777), false},
		{"symlink", statAnswer(1000, fs.ModeSymlink|0o777), false},
	} {
		err := checkPrivate(test.info, "/tmp/steps-shim", 1000)
		if (err == nil) != test.ok {
			t.Errorf("%s: checkPrivate = %v, want ok=%v", test.name, err, test.ok)
		}

		if err != nil && !errors.Is(err, errShimNotPrivate) {
			t.Errorf("%s: %v, want errShimNotPrivate", test.name, err)
		}
	}
}

// TestCheckRootRefusesARootAnotherLoginCanRearrange pins the parent of
// steps-shim/: whoever can write it can rename the checked directory away and
// put their own in its place, so a shared root must be sticky, and owned by
// this login or root.
func TestCheckRootRefusesARootAnotherLoginCanRearrange(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		info fs.FileInfo
		ok   bool
	}{
		{"/tmp: root-owned, sticky, world-writable", statAnswer(0, fs.ModeDir|fs.ModeSticky|0o777), true},
		{"own private root", statAnswer(1000, fs.ModeDir|0o700), true},
		{"root-owned 0755", statAnswer(0, fs.ModeDir|0o755), true},
		{"world-writable without sticky", statAnswer(0, fs.ModeDir|0o777), false},
		{"group-writable without sticky", statAnswer(1000, fs.ModeDir|0o775), false},
		{"another login's root", statAnswer(1001, fs.ModeDir|0o755), false},
	} {
		err := checkRoot(test.info, "/var/tmp/shared", 1000)
		if (err == nil) != test.ok {
			t.Errorf("%s: checkRoot = %v, want ok=%v", test.name, err, test.ok)
		}

		if err != nil && !errors.Is(err, errShimNotPrivate) {
			t.Errorf("%s: %v, want errShimNotPrivate", test.name, err)
		}
	}
}

// TestWriteRemoteStagesOwnerOnly pins the staging file's mode from the moment
// it exists, and that a name already taken is refused rather than reused: the
// later chmod cannot revoke a descriptor another login opened in between.
func TestWriteRemoteStagesOwnerOnly(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)

	client, err := sftp.NewClient(t.Context(), dialTestSSHD(t, server))
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	source := shimSource{ //nolint:exhaustruct // writeRemote reads the bytes and the name
		name: "shim",
		open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("shim bytes")), nil },
	}

	staging := filepath.Join(server.Root, "steps.part")

	err = writeRemote(client, source, staging)
	if err != nil {
		t.Fatalf("writeRemote: %v", err)
	}

	info, err := os.Stat(staging)
	if err != nil || info.Mode().Perm() != shimMode {
		t.Errorf("staging = %v, %v, want mode %o before any chmod", info, err, shimMode)
	}

	err = writeRemote(client, source, staging)
	if err == nil {
		t.Error("writeRemote reused a staging name that already existed")
	}
}

// dialTestSSHD is an SSH client to the in-process sshd, for a test of what
// the push does before it has anything running there.
func dialTestSSHD(t *testing.T, server *testSSHD) *ssh.Client {
	t.Helper()

	worker, err := ParseWorker(server.URL)
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	settings, err := connectionFor(worker)
	if err != nil {
		t.Fatalf("connectionFor: %v", err)
	}

	config, err := sshConfig(t.Context(), settings)
	if err != nil {
		t.Fatalf("sshConfig: %v", err)
	}

	dialer := net.Dialer{Timeout: dialTimeout}

	conn, err := dialer.DialContext(t.Context(), "tcp", settings.address)
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}

	sshConn, channels, requests, err := ssh.NewClientConn(conn, settings.address, config)
	if err != nil {
		_ = conn.Close()

		t.Fatalf("connecting: %v", err)
	}

	client := ssh.NewClient(sshConn, channels, requests)
	t.Cleanup(func() { _ = client.Close() })

	return client
}

func TestProbeWorkerReadsPlatformAndUID(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)
	// A login shell's rc noise ahead of the answer.
	server.RewriteExec(func(string) string { return "echo welcome; echo Linux aarch64; echo 1234" })

	probe := probeWorker(t.Context(), dialTestSSHD(t, server))
	if !probe.known || probe.goos != "linux" || probe.goarch != "arm64" || probe.uid != 1234 {
		t.Errorf("probe = %+v, want linux/arm64 uid 1234 past the noise", probe)
	}
}

// TestProbeWorkerGivesUpOnAWedgedWorker pins that the probe is bounded by
// its context, and leaves nothing running when it gives up.
func TestProbeWorkerGivesUpOnAWedgedWorker(t *testing.T) {
	t.Parallel()

	server := newTestSSHD(t)
	// Deaf to stdin's EOF, which the client sends at once; ends on its own
	// only after the bound below, so the server can shut down.
	server.RewriteExec(func(string) string { return "sleep 5" })

	client := dialTestSSHD(t, server)

	ctx, cancel := context.WithTimeout(t.Context(), shortWait)
	defer cancel()

	started := time.Now()
	probe := probeWorker(ctx, client)

	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("probe took %s, want it to give up with its context", elapsed)
	}

	if probe.known || probe.uid != -1 {
		t.Errorf("probe = %+v, want nothing learned", probe)
	}
}
