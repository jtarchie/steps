package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/testdocker"
)

// serviceRedis is a pinned redis a machine running the docker suite usually holds already.
const serviceRedis = "redis:7-alpine@sha256:7f7d30b7fdd09467bac58198a1f166dd69789d348d4fe859cccb20e0cf287fbe"

func serviceNames(t *testing.T) (string, string) {
	t.Helper()

	raw := make([]byte, 4)
	_, _ = rand.Read(raw)
	suffix := hex.EncodeToString(raw)

	container, network := "steps-test-redis-"+suffix, "steps-test-net-"+suffix
	testdocker.RemoveService(t, container, network)

	return container, network
}

func servicePipeline(container, network, env string) string {
	return `
resources:
- name: cache
  type: docker-service
  source:
    container: ` + container + `
    image: ` + serviceRedis + `
    network: ` + network + `
    ready: [redis-cli, ping]
` + env + `
jobs:
- name: up
  plan:
  - put: cache
  - task: ping
    image: ` + serviceRedis + `
    network: ` + network + `
    run: redis-cli -h ` + container + ` ping
    assert:
      stdout: PONG
`
}

// TestEndToEndADockerServiceIsUpForTheStepsOnItsNetwork: a put starts the container on its network and returns once ready answers, so the next step reaches it by name; a second put with nothing changed leaves the same container running, and a changed source: replaces it.
func TestEndToEndADockerServiceIsUpForTheStepsOnItsNetwork(t *testing.T) {
	requireDockerE2E(t)

	container, network := serviceNames(t)
	dir := t.TempDir()

	path := writePipeline(t, dir, servicePipeline(container, network, ""))
	mustRun(t, "run", path, "--job", "up", "--force")

	first := testdocker.ContainerID(t, container)
	if first == "" {
		t.Fatalf("no container %s after the put", container)
	}

	mustRun(t, "run", path, "--job", "up", "--force")

	if again := testdocker.ContainerID(t, container); again != first {
		t.Errorf("an unchanged put replaced the container: %s, then %s", first, again)
	}

	writePipeline(t, dir, servicePipeline(container, network, "    env: {REDIS_ARGS: --maxmemory 64mb}\n"))
	mustRun(t, "run", path, "--job", "up", "--force")

	if changed := testdocker.ContainerID(t, container); changed == first || changed == "" {
		t.Errorf("a changed source: kept the container %s (now %q), want it replaced", first, changed)
	}
}

// TestEndToEndADockerServiceThatNeverGetsReadyFailsThePut: a ready command that never succeeds fails the put, naming it, rather than letting the next step meet a service that is not there.
func TestEndToEndADockerServiceThatNeverGetsReadyFailsThePut(t *testing.T) {
	requireDockerE2E(t)

	container, network := serviceNames(t)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: cache
  type: docker-service
  source:
    container: `+container+`
    image: `+serviceRedis+`
    network: `+network+`
    ready: ["false"]
    ready_timeout: 3s
jobs:
- name: up
  plan:
  - put: cache
`)

	err := stepsRun(t, []string{"run", path, "--job", "up"})
	if err == nil || !strings.Contains(err.Error(), "never became ready") {
		t.Errorf("run: err = %v, want the put to say the service never became ready", err)
	}
}

// TestValidateRefusesAPlacedStepOnAServicesNetwork: a service runs on this machine's daemon, so a step placed on a worker cannot reach its network.
func TestValidateRefusesAPlacedStepOnAServicesNetwork(t *testing.T) {
	t.Parallel()

	path := writePipeline(t, t.TempDir(), `
resources:
- name: cache
  type: docker-service
  source: {container: c, image: `+serviceRedis+`, network: svc}
jobs:
- name: up
  plan:
  - put: cache
  - task: ping
    image: alpine:3
    network: svc
    tags: [remote]
    run: "true"
`)

	err := stepsRun(t, []string{"validate", "--syntax-only", path})
	if err == nil || !strings.Contains(err.Error(), `network "svc"`) || !strings.Contains(err.Error(), "docker-service") {
		t.Errorf("validate: err = %v, want the placed step on the service's network refused", err)
	}
}
