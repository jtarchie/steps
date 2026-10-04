package testsshd_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/testsshd"
)

func dial(t *testing.T, server *testsshd.Server) *ssh.Client {
	t.Helper()

	pem, err := os.ReadFile(server.Identity)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		t.Fatal(err)
	}

	client, err := ssh.Dial("tcp", server.Addr(), &ssh.ClientConfig{
		User:            "steps",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(server.HostKey),
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatalf("dialing the test sshd: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// shortSocketPath stays under macOS's 104-byte sun_path limit, which t.TempDir's long names overrun.
func shortSocketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "sshd")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return filepath.Join(dir, "s")
}

func TestStreamLocalReachesAUnixSocket(t *testing.T) {
	t.Parallel()

	path := shortSocketPath(t)

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		conn, err := listener.Accept()
		if err != nil {
			return
		}

		_, _ = io.Copy(conn, conn)
		_ = conn.Close()
	}()

	t.Cleanup(func() {
		_ = listener.Close()
		<-done
	})

	server := testsshd.New(t)

	conn, err := dial(t, server).Dial("unix", path)
	if err != nil {
		t.Fatalf("direct-streamlocal: %v", err)
	}

	_, err = io.WriteString(conn, "ping\n")
	if err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || line != "ping\n" {
		t.Fatalf("echo: %q, %v", line, err)
	}

	_ = conn.Close()

	if got := server.StreamLocals.Load(); got != 1 {
		t.Fatalf("StreamLocals = %d, want 1", got)
	}
}

// A missing daemon socket behind a live sshd must refuse the channel, so a venue can name it before any file moves.
func TestStreamLocalRefusesAMissingSocket(t *testing.T) {
	t.Parallel()

	server := testsshd.New(t)

	_, err := dial(t, server).Dial("unix", shortSocketPath(t))

	var openErr *ssh.OpenChannelError
	if !errors.As(err, &openErr) || openErr.Reason != ssh.ConnectionFailed {
		t.Fatalf("want a ConnectionFailed refusal, got %v", err)
	}

	if got := server.StreamLocals.Load(); got != 0 {
		t.Fatalf("StreamLocals = %d after a refusal", got)
	}
}

// The docker+ssh:// data plane in one hop: the engine API answering through the forward.
func TestStreamLocalCarriesTheDockerEngineAPI(t *testing.T) {
	t.Parallel()

	socket := dockerSocket(t)
	client := dial(t, testsshd.New(t))

	httpClient := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return client.Dial("unix", socket)
		},
	}}
	defer httpClient.CloseIdleConnections()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://docker/_ping", nil)
	if err != nil {
		t.Fatal(err)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatalf("GET /_ping through the forward: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != "OK" {
		t.Fatalf("/_ping: %d %q", response.StatusCode, body)
	}
}

func dockerSocket(t *testing.T) string {
	t.Helper()

	_, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not found on PATH")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	host, err := dockerapi.ResolveHost()
	if err != nil {
		t.Skipf("no docker endpoint: %v", err)
	}

	if !strings.HasPrefix(host, "unix://") {
		t.Skipf("docker endpoint %q is not a unix socket", host)
	}

	err = exec.CommandContext(ctx, "docker", "info").Run()
	if err != nil {
		t.Skip("docker daemon not reachable (`docker info` failed)")
	}

	return strings.TrimPrefix(host, "unix://")
}
