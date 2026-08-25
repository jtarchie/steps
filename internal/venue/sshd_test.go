package venue

// An SSH server in this process, so the ssh: venue is testable without a
// network, a credential, a daemon, or a second machine.
//
// It is a real sshd in every way the venue can observe: public-key auth, a
// session channel that execs through a shell, the sftp subsystem, and an
// exit-status reply. The venue is not talking to a mock — the only thing
// missing is OpenSSH's own configuration surface, which is exactly the part
// this cannot pin (see TestSSHConfigAgainstRealOpenSSH for the opt-in that
// can).

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

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type testSSHD struct {
	// URL is a worker mapping pointing at this server, with the generated key
	// and known_hosts already named.
	URL string
	// Root is the worker's home: where a pushed binary and its scratch land.
	Root string
	// HostKey is this server's own public key, so a test can pin it the way an
	// operator pins a machine that has no known_hosts entry.
	HostKey ssh.PublicKey
	// Identity is the private key a client authenticates with.
	Identity string
	// KnownHosts is the file holding this server's host key, so a test can
	// name it the way an operator's ssh_config names one.
	KnownHosts string

	// Uploads counts files written over sftp — how a test asks whether the
	// binary was pushed, and whether a second session reused it.
	Uploads atomic.Int64
	// Execs counts session channels opened.
	Execs atomic.Int64
	// EnvRequests counts SSH env requests, which must stay zero: OpenSSH
	// ignores them unless AcceptEnv names the variable, so a venue that
	// shipped a step's environment that way would work here and fail in
	// production.
	EnvRequests atomic.Int64

	listener net.Listener
	conns    sync.WaitGroup
	closed   atomic.Bool
}

func newTestSSHD(t *testing.T) *testSSHD {
	t.Helper()

	dir := t.TempDir()
	server := &testSSHD{Root: filepath.Join(dir, "worker")}

	err := os.MkdirAll(server.Root, 0o750)
	if err != nil {
		t.Fatalf("making the worker root: %v", err)
	}

	hostSigner, hostPub, _ := generateKey(t)
	_, clientPub, clientPriv := generateKey(t)

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientPub.Marshal()) {
				return nil, errors.New("unknown key")
			}

			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(hostSigner)

	var listenConfig net.ListenConfig

	server.listener, err = listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	identity := filepath.Join(dir, "id_ed25519")
	writeKey(t, identity, clientPriv)

	knownHosts := filepath.Join(dir, "known_hosts")
	writeKnownHosts(t, knownHosts, server.listener.Addr().String(), hostPub)

	server.HostKey = hostPub
	server.Identity = identity
	server.KnownHosts = knownHosts
	// ssh_config=none: this mapping says everything about how to reach the
	// server, and a test that also read the config of whoever ran it would
	// pass or fail by their Host * block.
	server.URL = fmt.Sprintf("ssh://%s/%s?identity=%s&known_hosts=%s&ssh_config=none",
		server.listener.Addr().String(), server.Root, identity, knownHosts)

	go server.accept(config)

	t.Cleanup(func() {
		server.closed.Store(true)
		_ = server.listener.Close()
		server.conns.Wait()
	})

	return server
}

func (s *testSSHD) accept(config *ssh.ServerConfig) {
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

func (s *testSSHD) serve(conn net.Conn, config *ssh.ServerConfig) {
	sshConn, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		_ = conn.Close()

		return
	}
	defer func() { _ = sshConn.Close() }()

	go ssh.DiscardRequests(requests)

	var open sync.WaitGroup

	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "only session channels")

			continue
		}

		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}

		open.Add(1)

		go func() {
			defer open.Done()

			s.session(channel, channelRequests)
		}()
	}

	open.Wait()
}

// session answers the two request types a venue uses: exec, and the sftp
// subsystem.
func (s *testSSHD) session(channel ssh.Channel, requests <-chan *ssh.Request) {
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
			// Recorded and ignored, exactly as OpenSSH does without AcceptEnv.
			s.EnvRequests.Add(1)
			_ = request.Reply(false, nil)
		default:
			_ = request.Reply(false, nil)
		}
	}
}

func (s *testSSHD) runExec(channel ssh.Channel, command string) {
	// Through a shell, because that is what sshd does: the venue's exec string
	// is handed to a login shell on the far end, and pretending otherwise
	// would hide a quoting bug rather than catch one.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", command) //nolint:gosec // a test server running the command the venue asked for
	cmd.Stdout = channel
	cmd.Stderr = channel.Stderr()

	// Stdin through a pipe fed by a goroutine, not by handing exec.Cmd the
	// channel directly. Given an io.Reader, exec.Cmd copies it on a goroutine
	// that Run WAITS for — and a copy from a channel the client is holding
	// open never ends, so Run would block long after the command exited. Real
	// sshd has no such problem; only this stand-in does, and getting it wrong
	// makes a dead command look like a hung one.
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

func (s *testSSHD) runSFTP(channel ssh.Channel) {
	server, err := sftp.NewServer(channel)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()

	// Counting writes rather than opens: the question a test asks is whether
	// the binary travelled, and a stat that decides it need not is the answer
	// the cache is supposed to give.
	_ = server.Serve()
}

// countingUpload is how runSFTP reports a write. sftp.Server handles the
// protocol itself, so the count comes from watching the filesystem instead:
// see uploadsUnder.
func uploadsUnder(t *testing.T, root string) int {
	t.Helper()

	count := 0

	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !entry.IsDir() && entry.Name() == "steps" {
			count++
		}

		return nil
	})
	if err != nil {
		t.Fatalf("counting pushed binaries: %v", err)
	}

	return count
}

func generateKey(t *testing.T) (ssh.Signer, ssh.PublicKey, ed25519.PrivateKey) {
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

func writeKey(t *testing.T, path string, key ed25519.PrivateKey) {
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

func writeKnownHosts(t *testing.T, path, address string, pub ssh.PublicKey) {
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

// commandOf reads the string an exec or subsystem request carries.
func commandOf(payload []byte) string {
	var request struct{ Value string }

	err := ssh.Unmarshal(payload, &request)
	if err != nil {
		return ""
	}

	return request.Value
}

// URLWithPin is a mapping at this server verified by a fingerprint instead of
// a known_hosts file — the shape a venue with no history takes.
func (s *testSSHD) URLWithPin(t *testing.T, fingerprint string) string {
	t.Helper()

	return fmt.Sprintf("ssh://%s/%s?identity=%s&hostkey=%s&ssh_config=none",
		s.listener.Addr().String(), s.Root, s.Identity, url.QueryEscape(fingerprint))
}

// URLWithHostKeyPin pins this server's actual key, which must be accepted.
func (s *testSSHD) URLWithHostKeyPin(t *testing.T) string {
	t.Helper()

	return s.URLWithPin(t, ssh.FingerprintSHA256(s.HostKey))
}
