package e2e

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/testsshd"
)

// dockerSSHWorker is a docker+ssh:// mapping at an in-process sshd that forwards to this machine's daemon, and the sshd itself.
func dockerSSHWorker(t *testing.T) (string, *testsshd.Server) {
	t.Helper()

	requireDockerE2E(t)

	host, err := dockerapi.ResolveHost()
	if err != nil {
		t.Skipf("resolving the docker host: %v", err)
	}

	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		t.Skipf("docker endpoint %q is not a unix socket", host)
	}

	server := testsshd.New(t)
	query := url.Values{
		"identity":    {server.Identity},
		"known_hosts": {server.KnownHosts},
		"ssh_config":  {"none"},
		"sock":        {socket},
	}

	return "docker+ssh://" + server.Addr() + "?" + query.Encode(), server
}

// The whole feature as a user sees it: a placed task runs in a container on the worker's daemon, its output comes home, and nothing but the step's files ever reaches the worker.
func TestDockerSSHWorkerRunsAPlacedStep(t *testing.T) {
	worker, server := dockerSSHWorker(t)

	dir := t.TempDir()
	published := filepath.Join(dir, "published.txt")

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: prepare
    outputs: [data]
    run: echo seed > data/seed.txt

  - task: remote
    tags: [box]
    image: `+dockerE2EImage+`
    inputs: [data]
    outputs: [report]
    run: |
      cat data/seed.txt > report/out.txt
      cat /etc/alpine-release >> report/out.txt
      printf '%s' "$STEPS_WORKER" > report/where.txt

  - task: publish
    inputs: [report]
    run: cp report/out.txt `+published+` && cp report/where.txt `+published+`.where
`)

	mustRun(t, path, "--worker", "box="+worker)

	got := readFileString(t, published)
	if !strings.Contains(got, "seed") || !strings.Contains(got, "3.") {
		t.Errorf("published = %q, want the input and alpine's release file: the step did not run in the image with its tree", got)
	}

	if where := readFileString(t, published+".where"); where != "box" {
		t.Errorf("STEPS_WORKER = %q, want box", where)
	}

	if execs := server.Execs.Load(); execs != 0 {
		t.Errorf("the worker ran %d commands over ssh; a docker+ worker gets nothing but its daemon's socket", execs)
	}

	remote := placementNamed(t, runPlacements(t, path), "remote")
	if !strings.HasPrefix(remote.Address, "docker+ssh://") {
		t.Errorf("placement address = %q", remote.Address)
	}

	if remote.BytesSent == 0 {
		t.Errorf("placement sent nothing: the step's input never reached the worker")
	}
}

// The issue's test (#206): a placed task writes an output, a second placed step on the same worker reads it, and the shared input crosses once — when the local publish pulls it, never between the two.
func TestDockerSSHWorkerKeepsWhatItProduces(t *testing.T) {
	worker, _ := dockerSSHWorker(t)

	dir := t.TempDir()
	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: seed
    tags: [a]
    image: `+dockerE2EImage+`
    outputs: [src]
    run: |
      head -c `+strconv.Itoa(payloadBytes)+` /dev/urandom > src/blob.bin
      printf '%s' "$STEPS_WORKER" > src/where.txt
  - task: consume
    tags: [a]
    image: `+dockerE2EImage+`
    inputs: [src]
    outputs: [out]
    run: |
      wc -c < src/blob.bin | tr -d ' ' > out/size.txt
      cp src/where.txt out/where.txt
  - task: publish
    inputs: [out]
    run: cp out/size.txt `+filepath.Join(dir, "size.txt")+` && cp out/where.txt `+filepath.Join(dir, "where.txt")+`
`)

	mustRun(t, path, "--worker", "a="+worker)

	assertPublished(t, dir)

	placements := runPlacements(t, path)

	if sent := placementNamed(t, placements, "consume").BytesSent; sent >= payloadBytes {
		t.Errorf("consume sent %d bytes to the worker that produced them; want under the payload", sent)
	}

	for _, name := range []string{"seed", "consume"} {
		if got := placementNamed(t, placements, name).BytesReceived; got != 0 {
			t.Errorf("%s brought %d bytes home when it finished; a held output comes home only when read here", name, got)
		}
	}
}

// removeCacheVolumes drops what this process's docker+ workers kept, once its tests are done.
func removeCacheVolumes() {
	ctx := context.Background()

	//nolint:gosec // a filter built from this process's own pid
	out, err := exec.CommandContext(ctx, "docker", "volume", "ls", "-q", "--filter", "label=steps.pid="+strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return
	}

	// The cache, and the held outputs it names: everything a closed session leaves on purpose.
	for _, name := range strings.Fields(string(out)) {
		if strings.HasPrefix(name, "steps-a-") || strings.HasPrefix(name, "steps-d-") || strings.HasPrefix(name, "steps-out-") {
			_ = exec.CommandContext(ctx, "docker", "volume", "rm", name).Run() //nolint:gosec // a name the daemon listed
		}
	}
}
