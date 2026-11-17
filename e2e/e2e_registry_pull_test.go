package e2e

import (
	"archive/tar"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/testdocker"
)

// pullTestImage is an image with a shell, rebuilt into a private registry for a step to run in. treedigest's pin, so a machine that runs the docker suite already holds it.
const pullTestImage = "busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

// privateImage pushes pullTestImage's filesystem, as a one-layer image for the daemon's platform, to a fresh private registry, and returns its repository there and the digest it has. Read out of the daemon rather than copied from Docker Hub, whose anonymous pull limit this suite would otherwise share with everything else on the machine.
func privateImage(t *testing.T, extra ...v1.Layer) (string, string) {
	t.Helper()

	rootfs, os, arch := testdocker.RootFS(t, pullTestImage)
	host := testdocker.Registry(t)
	repository := host + "/private/busybox"

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(rootfs)), nil })
	if err != nil {
		t.Fatal(err)
	}

	image, err := mutate.AppendLayers(empty.Image, append([]v1.Layer{layer}, extra...)...)
	if err != nil {
		t.Fatal(err)
	}

	image, err = mutate.ConfigFile(image, &v1.ConfigFile{OS: os, Architecture: arch, Config: v1.Config{Env: []string{"PATH=/bin:/usr/bin"}}, RootFS: mustRootFS(t, image)})
	if err != nil {
		t.Fatal(err)
	}

	target, err := name.NewTag(repository + ":latest")
	if err != nil {
		t.Fatal(err)
	}

	// Its own transport, closed with the test: the shared one would keep a connection alive past it, which goleak reports.
	transport, _ := remote.DefaultTransport.(*http.Transport)
	transport = transport.Clone()
	t.Cleanup(transport.CloseIdleConnections)

	err = remote.Write(target, image, remote.WithContext(t.Context()), remote.WithTransport(transport), remote.WithAuth(&authn.Basic{Username: testdocker.RegistryUsername, Password: testdocker.RegistryPassword}))
	if err != nil {
		t.Fatalf("pushing into the private registry: %v", err)
	}

	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}

	return repository, digest.String()
}

// mustRootFS is the diff ids an image's config must list, read back from the image the layers were appended to.
func mustRootFS(t *testing.T, image v1.Image) v1.RootFS {
	t.Helper()

	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}

	return config.RootFS
}

// TestEndToEndAStepRunsInAPrivateImageWithTheResourcesLogin: the daemon pulls a step's image: <get> with the login that resource names, and nothing else; no docker config on this machine knows the registry.
func TestEndToEndAStepRunsInAPrivateImageWithTheResourcesLogin(t *testing.T) {
	requireDockerE2E(t)
	t.Setenv("PRIVATE_REGISTRY_PASSWORD", testdocker.RegistryPassword)
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	repository, _ := privateImage(t)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: toolchain
  type: registry-image
  source:
    repository: `+repository+`
    username: `+testdocker.RegistryUsername+`
    password_env: PRIVATE_REGISTRY_PASSWORD
jobs:
- name: build
  plan:
  - get: toolchain
  - task: inside
    image: toolchain
    run: echo "ran in $(uname -s)"
`)

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "build") })
	if !strings.Contains(out, "ran in Linux") {
		t.Errorf("run printed %q, want the task to have run in the private image", out)
	}
}

// TestEndToEndAPrivateImageByNameSaysWhereTheLoginGoes: an image: string pulls anonymously, so a private one is refused by its registry, and the error says to fetch it through a registry-image get rather than to log docker in.
func TestEndToEndAPrivateImageByNameSaysWhereTheLoginGoes(t *testing.T) {
	requireDockerE2E(t)

	repository, digest := privateImage(t)

	path := writePipeline(t, t.TempDir(), `
jobs:
- name: build
  plan:
  - task: inside
    image: `+repository+`@`+digest+`
    run: "true"
`)

	err := stepsRun(t, []string{"run", path, "--job", "build"})
	if err == nil || !strings.Contains(err.Error(), "registry-image") || !strings.Contains(err.Error(), "docker's config.json") {
		t.Errorf("run: err = %v, want the refusal to point at a registry-image get", err)
	}
}

// concourseScripts is a layer holding a Concourse resource's three executables: a check that finds one version, an in that fetches it, an out that publishes nothing.
func concourseScripts(t *testing.T) v1.Layer {
	t.Helper()

	var buffer bytes.Buffer

	writer := tar.NewWriter(&buffer)

	for _, dir := range []string{"opt/", "opt/resource/"} {
		err := writer.WriteHeader(&tar.Header{Name: dir, Typeflag: tar.TypeDir, Mode: 0o755})
		if err != nil {
			t.Fatal(err)
		}
	}

	for script, body := range map[string]string{
		"check": "#!/bin/sh\ncat > /dev/null\necho '[{\"v\":\"1\"}]'\n",
		"in":    "#!/bin/sh\ncat > /dev/null\necho private > \"$1/said\"\necho '{\"version\":{\"v\":\"1\"}}'\n",
		"out":   "#!/bin/sh\ncat > /dev/null\necho '{\"version\":{\"v\":\"1\"}}'\n",
	} {
		err := writer.WriteHeader(&tar.Header{Name: "opt/resource/" + script, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
		if err != nil {
			t.Fatal(err)
		}

		_, err = writer.Write([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
	}

	err := writer.Close()
	if err != nil {
		t.Fatal(err)
	}

	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buffer.Bytes())), nil })
	if err != nil {
		t.Fatal(err)
	}

	return layer
}

// TestEndToEndAPrivateResourceTypeImagePullsWithItsLogin: a resource_types: entry naming a Concourse resource image in a private registry gives the login beside source.repository, and the daemon pulls the type's image with it.
func TestEndToEndAPrivateResourceTypeImagePullsWithItsLogin(t *testing.T) {
	requireDockerE2E(t)
	t.Setenv("PRIVATE_TYPE_PASSWORD", testdocker.RegistryPassword)

	repository, _ := privateImage(t, concourseScripts(t))

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: private-type
  type: registry-image
  source:
    repository: `+repository+`
    username: `+testdocker.RegistryUsername+`
    password_env: PRIVATE_TYPE_PASSWORD
resources:
- name: thing
  type: private-type
  source: {}
jobs:
- name: build
  plan:
  - get: thing
  - task: show
    inputs: [thing]
    run: cat thing/said
`)

	out := captureStdout(t, func() { mustRun(t, "run", path, "--job", "build") })
	if !strings.Contains(out, "private") {
		t.Errorf("run printed %q, want the private type's in: to have run", out)
	}
}

// TestEndToEndAPolledPrivateResourceTypePullsWithItsLogin: under steps web the type's image is first pulled by the poll's own check, outside any run and its up-front pulls, so the login has to ride on the check itself.
func TestEndToEndAPolledPrivateResourceTypePullsWithItsLogin(t *testing.T) {
	requireDockerE2E(t)
	t.Setenv("PRIVATE_TYPE_PASSWORD", testdocker.RegistryPassword)

	repository, _ := privateImage(t, concourseScripts(t))

	path := writePipeline(t, t.TempDir(), `
resource_types:
- name: private-type
  type: registry-image
  source:
    repository: `+repository+`
    username: `+testdocker.RegistryUsername+`
    password_env: PRIVATE_TYPE_PASSWORD
resources:
- name: thing
  type: private-type
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
`)

	served := startWebFor(t, path, "--interval", "200ms")
	defer served.stopIfRunning(t)

	name := cli.PipelineName(path)
	waitForRunStatus(t, served.state, name, newestRun(t, served.state, name, "build").ID, "succeeded")
}
