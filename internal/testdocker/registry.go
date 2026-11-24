package testdocker

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
)

// The login every Registry answers to, and nothing else does.
const (
	RegistryUsername = "robot"
	RegistryPassword = "steps-test-registry-pw"
	// registryHtpasswd is RegistryUsername:RegistryPassword, bcrypt, as `htpasswd -nbB` writes it: the registry accepts nothing weaker.
	registryHtpasswd = "robot:$2y$05$d3F2DbkNW2OEpKCtBbz1G.EyuLs3OkPK3LRQh0NXaYGNNy9BBK20O" //nolint:gosec // a throwaway test registry's login, the point of the fixture
	registryImage    = "registry:2.8.3"
)

// Registry runs a private OCI registry on this machine's daemon, behind RegistryUsername and RegistryPassword, and returns the host both this process and the daemon reach it by: 127.0.0.1 and a published port, which a daemon in a VM (colima, Docker Desktop) forwards to the same loopback. Removed when the test ends.
//
// A container rather than an in-process server because the point of a test using it is that the DAEMON pulls from it, and a daemon in a VM cannot see a port this process opened.
func Registry(t testing.TB) string {
	t.Helper()

	client := dial(t)

	if !client.ImagePresent(t.Context(), registryImage) {
		err := client.Pull(t.Context(), registryImage, io.Discard)
		if err != nil {
			t.Skipf("cannot pull %s: %v", registryImage, err)
		}
	}

	id, err := client.CreateContainer(t.Context(), dockerapi.ContainerSpec{
		Image: registryImage,
		Name:  "steps-test-registry-" + suffix(t),
		// The file is written from the environment so nothing is mounted from a path, which steps refuses (volumesOnly).
		Cmd: []string{"sh", "-c", `mkdir -p /auth && printf '%s\n' "$HTPASSWD" > /auth/htpasswd && exec registry serve /etc/docker/registry/config.yml`},
		Env: []string{
			"HTPASSWD=" + registryHtpasswd,
			"REGISTRY_AUTH=htpasswd",
			"REGISTRY_AUTH_HTPASSWD_REALM=steps-test",
			"REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		},
		Publish: []string{"5000/tcp"},
	})
	if err != nil {
		t.Fatalf("creating the registry: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = client.RemoveContainer(ctx, id)
	})

	err = client.StartContainer(t.Context(), id)
	if err != nil {
		t.Fatalf("starting the registry: %v", err)
	}

	port, err := client.PublishedPort(t.Context(), id, "5000/tcp")
	if err != nil {
		t.Fatalf("the registry's port: %v", err)
	}

	host := net.JoinHostPort("127.0.0.1", port)
	waitForRegistry(t, host)

	return host
}

// waitForRegistry polls /v2/ until the registry answers at all; behind auth that is a 401, which is the answer meaning it is up.
func waitForRegistry(t testing.TB, host string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}

	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/v2/", nil)
		if err != nil {
			t.Fatal(err)
		}

		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()

			if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusOK {
				return
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("the registry at %s never answered: %v", host, err)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// RootFS is image's filesystem as a tar stream, and the platform the daemon runs it on, read out of a container that is created and never started. With it a test builds a runnable image of its own without asking any registry for one: Docker Hub's anonymous pull limit is shared by every test on the machine. Skips when the daemon does not hold image and cannot pull it.
func RootFS(t testing.TB, image string) ([]byte, string, string) {
	t.Helper()

	client := dial(t)

	if !client.ImagePresent(t.Context(), image) {
		err := client.Pull(t.Context(), image, io.Discard)
		if err != nil {
			t.Skipf("cannot pull %s: %v", image, err)
		}
	}

	id, err := client.CreateHolder(t.Context(), "steps-test-rootfs-"+suffix(t), image, nil, nil)
	if err != nil {
		t.Fatalf("creating a container of %s: %v", image, err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		_ = client.RemoveContainer(ctx, id)
	})

	archive, err := client.GetArchive(t.Context(), id, "/")
	if err != nil {
		t.Fatalf("reading %s's filesystem: %v", image, err)
	}

	defer func() { _ = archive.Close() }()

	content, err := io.ReadAll(archive)
	if err != nil {
		t.Fatalf("reading %s's filesystem: %v", image, err)
	}

	os, arch, err := client.Platform(t.Context())
	if err != nil {
		t.Fatalf("the daemon's platform: %v", err)
	}

	return content, os, arch
}
