// Package testsshd is an in-process sshd so worker venues are testable without a network, a credential or a second machine; only OpenSSH's own config surface is missing.
package testsshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pkg/sftp/v2"
	"github.com/pkg/sftp/v2/localfs"
	"golang.org/x/crypto/ssh"
)

// Server is a real sshd in every way a venue can observe: key auth, exec through a shell, sftp, exit-status, streamlocal.
type Server struct {
	URL        string
	Root       string
	HostKey    ssh.PublicKey
	Identity   string
	KnownHosts string

	Execs atomic.Int64
	// Must stay zero: OpenSSH ignores env requests without AcceptEnv, so a venue relying on them would pass here and fail in production.
	EnvRequests  atomic.Int64
	StreamLocals atomic.Int64

	listener net.Listener
	conns    sync.WaitGroup
}

// New accepts only the generated client key, and its URL names that key and the known_hosts holding this server's.
func New(t testing.TB) *Server {
	t.Helper()

	hostSigner, hostPub, _ := GenerateKey(t)
	_, clientPub, clientPriv := GenerateKey(t)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientPub.Marshal()) {
				return nil, errors.New("unknown key")
			}

			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(hostSigner)

	server := NewWithConfig(t, config, hostPub)
	dir := filepath.Dir(server.Root)

	server.Identity = filepath.Join(dir, "id_ed25519")
	writeKey(t, server.Identity, clientPriv)

	server.KnownHosts = filepath.Join(dir, "known_hosts")
	writeKnownHosts(t, server.KnownHosts, server.Addr(), hostPub)

	// ssh_config=none so the result never depends on the Host * block of whoever runs the test.
	server.URL = fmt.Sprintf("ssh://%s/%s?identity=%s&known_hosts=%s&ssh_config=none",
		server.Addr(), server.Root, server.Identity, server.KnownHosts)

	return server
}

// NewWithConfig serves config, whose auth and host key the caller chose; hostKey is the public half it added.
func NewWithConfig(t testing.TB, config *ssh.ServerConfig, hostKey ssh.PublicKey) *Server {
	t.Helper()

	server := &Server{Root: filepath.Join(t.TempDir(), "worker"), HostKey: hostKey}

	err := os.MkdirAll(server.Root, 0o750)
	if err != nil {
		t.Fatalf("making the worker root: %v", err)
	}

	var listenConfig net.ListenConfig

	server.listener, err = listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	go server.accept(config)

	t.Cleanup(func() {
		_ = server.listener.Close()
		server.conns.Wait()
	})

	return server
}

// Addr is host:port, for a test that builds its own mapping.
func (s *Server) Addr() string { return s.listener.Addr().String() }

// URLWithPin verifies the server by fingerprint instead of known_hosts, as a venue with no history does.
func (s *Server) URLWithPin(t testing.TB, fingerprint string) string {
	t.Helper()

	return fmt.Sprintf("ssh://%s/%s?identity=%s&hostkey=%s&ssh_config=none",
		s.Addr(), s.Root, s.Identity, url.QueryEscape(fingerprint))
}

// URLWithHostKeyPin pins this server's actual key, which must be accepted.
func (s *Server) URLWithHostKeyPin(t testing.TB) string {
	t.Helper()

	return s.URLWithPin(t, ssh.FingerprintSHA256(s.HostKey))
}

func (s *Server) accept(config *ssh.ServerConfig) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		s.conns.Add(1)

		go func() {
			defer s.conns.Done()

			s.serve(conn, config)
		}()
	}
}

func (s *Server) serve(conn net.Conn, config *ssh.ServerConfig) {
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()

		return
	}
	defer func() { _ = sshConn.Close() }()

	go ssh.DiscardRequests(requests)

	var open sync.WaitGroup

	for newChannel := range channels {
		switch newChannel.ChannelType() {
		case "session":
			channel, channelRequests, err := newChannel.Accept()
			if err != nil {
				continue
			}

			open.Go(func() { s.session(channel, channelRequests) })
		case "direct-streamlocal@openssh.com":
			open.Go(func() { s.streamLocal(newChannel) })
		default:
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session and direct-streamlocal channels")
		}
	}

	open.Wait()
}

// streamLocal forwards to whatever socket the client names, as OpenSSH does with AllowStreamLocalForwarding's default.
func (s *Server) streamLocal(newChannel ssh.NewChannel) {
	var request struct {
		SocketPath string
		Reserved0  string
		Reserved1  uint32
	}

	err := ssh.Unmarshal(newChannel.ExtraData(), &request)
	if err != nil {
		_ = newChannel.Reject(ssh.Prohibited, "malformed direct-streamlocal request")

		return
	}

	var dialer net.Dialer

	socket, err := dialer.DialContext(context.Background(), "unix", request.SocketPath)
	if err != nil {
		_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())

		return
	}
	defer func() { _ = socket.Close() }()

	channel, channelRequests, err := newChannel.Accept()
	if err != nil {
		return
	}
	defer func() { _ = channel.Close() }()

	s.StreamLocals.Add(1)

	// Requests close with the channel, not on EOF: a client that hangs up closes the socket as OpenSSH does, while a half-close still only half-closes.
	go func() {
		ssh.DiscardRequests(channelRequests)
		_ = socket.Close()
	}()

	var copies sync.WaitGroup

	// Half-close each way, so an HTTP hijack (docker attach) still sees EOF on stdin.
	copies.Go(func() {
		_, _ = io.Copy(socket, channel)
		_ = socket.(*net.UnixConn).CloseWrite()
	})
	copies.Go(func() {
		_, _ = io.Copy(channel, socket)
		_ = channel.CloseWrite()
	})
	copies.Wait()
}

func (s *Server) session(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer func() { _ = channel.Close() }()

	for request := range requests {
		switch request.Type {
		case "exec":
			s.Execs.Add(1)

			_ = request.Reply(true, nil)
			s.runExec(channel, commandOf(request.Payload))

			return
		case "subsystem":
			if name := commandOf(request.Payload); name != "sftp" {
				_ = request.Reply(false, nil)

				continue
			}

			_ = request.Reply(true, nil)
			s.runSFTP(channel)

			return
		case "env":
			s.EnvRequests.Add(1)
			_ = request.Reply(false, nil)
		default:
			_ = request.Reply(false, nil)
		}
	}
}

func (s *Server) runExec(channel ssh.Channel, command string) {
	// Through a shell, as sshd does, so a quoting bug in the venue surfaces here.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command) //nolint:gosec // a test server running the command the venue asked for
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()

	// A pipe, not cmd.Stdin = channel: exec.Cmd waits on its stdin copy, which never ends while the client holds the channel open.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return
	}

	go func() {
		_, _ = io.Copy(stdin, channel)
		_ = stdin.Close()
	}()

	status := 0

	err = cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			status = exitErr.ExitCode()
		} else {
			status = 127
			_, _ = io.WriteString(channel.Stderr(), err.Error())
		}
	}

	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
}

func (s *Server) runSFTP(channel ssh.Channel) {
	server := &sftp.Server{Handler: &localfs.ServerHandler{}}
	defer func() { _ = server.GracefulStop() }()

	_ = server.Serve(channel)
}

// GenerateKey returns one fresh ed25519 key in each form a test needs.
func GenerateKey(t testing.TB) (ssh.Signer, ssh.PublicKey, ed25519.PrivateKey) {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("building a signer: %v", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("building a public key: %v", err)
	}

	return signer, sshPub, priv
}

func writeKey(t testing.TB, path string, key ed25519.PrivateKey) {
	t.Helper()

	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatalf("marshalling the key: %v", err)
	}

	err = os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
	if err != nil {
		t.Fatalf("writing the key: %v", err)
	}
}

func writeKnownHosts(t testing.TB, path, address string, pub ssh.PublicKey) {
	t.Helper()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("splitting %q: %v", address, err)
	}

	line := fmt.Sprintf("[%s]:%s %s\n", host, port, string(ssh.MarshalAuthorizedKey(pub)))

	err = os.WriteFile(path, []byte(line), 0o600)
	if err != nil {
		t.Fatalf("writing known_hosts: %v", err)
	}
}

func commandOf(payload []byte) string {
	var request struct{ Value string }

	err := ssh.Unmarshal(payload, &request)
	if err != nil {
		return ""
	}

	return request.Value
}
