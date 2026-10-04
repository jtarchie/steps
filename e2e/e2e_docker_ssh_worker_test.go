package e2e

import (
	"net/url"
	"path/filepath"
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

	if remote.BytesSent == 0 || remote.BytesReceived == 0 {
		t.Errorf("placement bytes sent %d, received %d: the tree did not cross both ways", remote.BytesSent, remote.BytesReceived)
	}
}
