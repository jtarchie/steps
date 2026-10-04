package testsshd

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A peer that never answers EOF must not pin the server: OpenSSH closes the socket when the channel closes.
func TestStreamLocalLetsGoOfASilentPeer(t *testing.T) {
	t.Parallel()

	dir, err := os.MkdirTemp("/tmp", "sshd")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	var listenConfig net.ListenConfig

	listener, err := listenConfig.Listen(t.Context(), "unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}

	server := New(t)

	peers := make(chan net.Conn, 1)

	go func() {
		conn, err := listener.Accept()
		if err == nil {
			peers <- conn
		}

		close(peers)
	}()

	// Registered after New, so it runs first and frees a failing test's server before New's cleanup waits on it.
	t.Cleanup(func() {
		_ = listener.Close()
		for peer := range peers {
			_ = peer.Close()
		}
	})

	client := clientOf(t, server)

	conn, err := client.Dial("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}

	_ = conn.Close()
	_ = client.Close()

	done := make(chan struct{})

	go func() {
		server.conns.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the server still holds a connection whose client hung up")
	}
}

func clientOf(t *testing.T, server *Server) *ssh.Client {
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
	})
	if err != nil {
		t.Fatal(err)
	}

	return client
}
