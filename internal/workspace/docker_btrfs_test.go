package workspace

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// unavailableMarker is printed (and the setup script exits 0, not an
// error) when a privileged container can start but the environment still
// can't actually mount a loopback btrfs image — e.g. Docker itself lacks
// the privilege, or the host kernel has no btrfs module. That's an
// environment limitation, not a code failure, so callers skip rather than
// fail when they see it.
const unavailableMarker = "__STEPS_DOCKER_BTRFS_UNAVAILABLE__"

// requireDocker skips the calling test unless it's explicitly opted into and
// a usable Docker daemon is reachable. These tests are heavyweight and
// non-hermetic — they pull a ~1GB image, apt-get packages over the network,
// and need a --privileged container to mount a loopback btrfs image — so
// they must NOT run as part of a plain `go test ./...` (which CLAUDE.md
// expects to finish in <5s). Set STEPS_TEST_DOCKER=1 to run them, mirroring
// how workspace_btrfs_linux_test.go gates real-btrfs tests behind
// STEPS_TEST_BTRFS_ROOT. The btrfs backend also has fast, hermetic unit
// coverage (workspace_test.go, via the copy backend, over the same shared
// isolatingProvider lifecycle); this suite exercises the btrfs code path
// specifically.
func requireDocker(t *testing.T) {
	t.Helper()

	if os.Getenv("STEPS_TEST_DOCKER") == "" {
		t.Skip("set STEPS_TEST_DOCKER=1 to run the Docker-backed btrfs tests (heavyweight: pulls an image, needs a privileged container)")
	}

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}
}

// runInDockerContainer runs script (via `bash -c`) inside a privileged
// golang:1.26-bookworm container with this repo mounted read-write at
// /src, returning combined stdout+stderr. --privileged is required to
// mkfs/mount a loopback btrfs image inside the container — the reason
// these tests exist instead of just shelling out to `go test` directly.
// Go module/build caches are kept in named volumes across runs so repeat
// test runs don't re-download the module graph every time.
func runInDockerContainer(t *testing.T, script string) (string, error) {
	t.Helper()

	repoDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	volCtx, volCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer volCancel()

	for _, vol := range []string{"steps-gocache", "steps-gomodcache"} {
		createErr := exec.CommandContext(volCtx, "docker", "volume", "create", vol).Run() //nolint:gosec // fixed argv, no injection surface
		if createErr != nil {
			t.Fatalf("docker volume create %s: %v", vol, createErr)
		}
	}

	args := []string{
		"run", "--rm", "--privileged", "-m", "4g",
		"-v", repoDir + ":/src",
		"-v", "steps-gocache:/root/.cache/go-build",
		"-v", "steps-gomodcache:/root/go/pkg/mod",
		"-w", "/src",
		"golang:1.26-bookworm",
		"bash", "-c", script,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // fixed image/mounts; script is a Go string literal, not user input

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()

	return out.String(), runErr
}

// provisionLoopbackBtrfsScript is the shared setup every test in this file
// runs first: install btrfs-progs, create a 512MB loopback image, format
// it, and mount it at /mnt/btrfs. It's shared as a shell snippet (not a Go
// helper) because it has to run inside the container, not on the host.
const provisionLoopbackBtrfsScript = `
set -e
apt-get update -qq && apt-get install -y -qq btrfs-progs >/dev/null
truncate -s 512M /tmp/btrfs.img
mkfs.btrfs -q /tmp/btrfs.img >/dev/null
mkdir -p /mnt/btrfs
if ! mount -o loop /tmp/btrfs.img /mnt/btrfs; then
  echo '` + unavailableMarker + `'
  exit 0
fi
`

// skipIfBtrfsUnavailable skips t if out shows the environment couldn't
// provision a loopback btrfs mount (see unavailableMarker), and fails t if
// the container run itself errored for any other reason.
func skipIfBtrfsUnavailable(t *testing.T, out string, err error) {
	t.Helper()

	if strings.Contains(out, unavailableMarker) {
		t.Skip("docker is available but this environment can't mount a loopback btrfs filesystem (needs --privileged support and a btrfs-capable kernel)")
	}

	if err != nil {
		t.Fatalf("docker container setup failed: %v\n%s", err, out)
	}
}

// TestDockerBtrfsGoTestSuite runs this repo's own btrfs-backend test suite
// (workspace_btrfs_linux_test.go, STEPS_TEST_BTRFS_ROOT-gated) inside the
// container against the real loopback filesystem it just provisioned —
// the same tests workspace_test.go/workspace_btrfs_linux_test.go define,
// just executed here instead of skipped.
func TestDockerBtrfsGoTestSuite(t *testing.T) {
	requireDocker(t)

	script := provisionLoopbackBtrfsScript + `
STEPS_TEST_BTRFS_ROOT=/mnt/btrfs go test ./... -run Btrfs -v -p 1
`

	out, err := runInDockerContainer(t, script)
	skipIfBtrfsUnavailable(t, out, err)

	if err != nil {
		t.Fatalf("btrfs backend test suite failed inside docker:\n%s", out)
	}

	if !strings.Contains(out, "PASS") || strings.Contains(out, "FAIL") {
		t.Errorf("expected a clean PASS with no FAIL in btrfs test output:\n%s", out)
	}

	if !strings.Contains(out, "TestBtrfsProviderCreatesRealSubvolumes") {
		t.Errorf("expected btrfs-specific tests to actually run (not just skip), got:\n%s", out)
	}
}

// TestDockerBtrfsCLIEndToEnd builds the real steps binary and runs an
// isolated pipeline (workspace: {strategy: btrfs}) against it inside the
// container, black-box: it never touches this package's internals, only
// the CLI's observable behavior — a task declaring inputs: [built] must
// not see the repo/ directory an earlier step fetched, and the whole run
// must succeed.
func TestDockerBtrfsCLIEndToEnd(t *testing.T) {
	requireDocker(t)

	const pipeline = `
workspace:
  strategy: btrfs
  root: /mnt/btrfs
  options:
    compression: zstd

resource_types:
- name: dummy
  config:
    check: echo '[{"ref":"v1"}]'
    in: echo hello > file.txt
    out: test -d built

resources:
- name: repo
  type: dummy
  source: {}
- name: results
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: build
    run: mkdir -p built && echo built > built/output.txt
    inputs: [repo]
    outputs: [built]
  - task: verify
    run: test -f built/output.txt && test ! -e repo && echo ISOLATION_OK
    inputs: [built]
  - put: results
    inputs: [built]
`

	script := provisionLoopbackBtrfsScript + `
mkdir -p /tmp/pipeline
cat > /tmp/pipeline/test.yml <<'PIPELINE_EOF'
` + pipeline + `
PIPELINE_EOF
go build -o /tmp/steps .
/tmp/steps --job build /tmp/pipeline/test.yml
`

	out, err := runInDockerContainer(t, script)
	skipIfBtrfsUnavailable(t, out, err)

	if err != nil {
		t.Fatalf("steps CLI run failed inside docker:\n%s", out)
	}

	if !strings.Contains(out, "ISOLATION_OK") {
		t.Errorf("expected the verify task to print ISOLATION_OK (proving it couldn't see repo/), got:\n%s", out)
	}
}
