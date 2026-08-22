package venue

// The ssh: venue: a worker reached over SSH.
//
// The remote contract is sshd and a pushed steps binary, and nothing else. No
// agent to install, no daemon to leave running, no package to add — which is
// what makes "point it at a machine you already have" true rather than
// aspirational.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// dialTimeout bounds reaching a worker. A machine that is down should fail the
// step promptly rather than hold a build open for the TCP default.
const dialTimeout = 30 * time.Second

// defaultSSHPort is appended when a worker URL names no port.
const defaultSSHPort = "22"

var (
	// errNoAuth is a worker with no way to authenticate to it.
	errNoAuth = errors.New("no SSH credentials: start an agent (ssh-add) or name a key with ?identity=")
	// errShimDidNotStart is a pushed binary that could not run on the worker,
	// which is nearly always an architecture mismatch.
	errShimDidNotStart = errors.New("the pushed steps binary did not start on the worker")
)

func dialSSH(ctx context.Context, worker Worker) (*transport, error) {
	config, err := sshConfig(ctx, worker)
	if err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: dialTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", addressOf(worker))
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	sshConn, channels, requests, err := ssh.NewClientConn(conn, addressOf(worker), config)
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("connecting: %w", err)
	}

	client := ssh.NewClient(sshConn, channels, requests)

	remote, err := pushShim(client, worker)
	if err != nil {
		_ = client.Close()

		return nil, err
	}

	return startShim(client, remote)
}

// startShim execs the pushed binary and hands back its stdio as the transport.
func startShim(client *ssh.Client, remote string) (*transport, error) {
	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("opening a session: %w", err)
	}

	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()

		return nil, fmt.Errorf("opening a pipe to the shim: %w", err)
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		_ = client.Close()

		return nil, fmt.Errorf("opening a pipe from the shim: %w", err)
	}

	// The channel's stderr is reserved for diagnostics and carries no protocol
	// bytes, ever. It is the only place a binary that could not exec — the
	// wrong architecture, a stripped loader — gets to say so, and keeping it
	// clear is what turns that into a usable message instead of a hang.
	diagnostics := &diagnosticBuffer{left: diagnosticBytes}
	session.Stderr = diagnostics

	// The remote login shell runs this string, so the path is deliberately one
	// no shell treats specially: a temp directory, a hex build key, and the
	// name steps. Everything else the shim needs arrives in the hello frame
	// rather than as an argument, which is one fewer thing to quote in a
	// dialect this end cannot see.
	err = session.Start(remote + " _shim")
	if err != nil {
		_ = session.Close()
		_ = client.Close()

		return nil, fmt.Errorf("starting the shim: %w", err)
	}

	// One waiter, started now rather than at close, for a reason that only
	// shows up on the failure path: a binary the worker cannot exec dies
	// immediately, and nothing else would notice. Reads on the channel do not
	// reliably end when the remote command does, so without this the handshake
	// waits out its whole timeout for a shim that was never going to speak.
	//
	// CLOSED rather than sent to. Two places ask whether the process ended —
	// the handshake, and the teardown — and a one-shot value is consumed by
	// whichever asks first, leaving the other waiting on something that will
	// never arrive again. A closed channel answers everyone, forever.
	exit := &sessionExit{done: make(chan struct{})}

	go func() {
		exit.err = session.Wait()
		_ = session.Close()
		close(exit.done)
	}()

	return &transport{
		in:          io.NopCloser(stdout),
		out:         stdin,
		diagnostics: diagnostics.String,
		exited:      exit.done,
		close: func(ctx context.Context) error {
			return closeSession(ctx, client, stdin, exit, diagnostics)
		},
	}, nil
}

// sessionExit is the remote process's ending, readable by everyone who asks.
// err is written before done is closed, so a reader that saw the close sees a
// settled value.
type sessionExit struct {
	done chan struct{}
	err  error
}

// closeSession says goodbye and collects the shim's exit.
func closeSession(ctx context.Context, client *ssh.Client, stdin io.WriteCloser, exit *sessionExit, diagnostics *diagnosticBuffer) error {
	// Closing stdin is the goodbye the shim listens for: it removes its
	// scratch and exits. It is also the ONLY cancellation that reliably
	// crosses SSH — sshd does not forward signal requests to an exec channel,
	// so a shim waiting on anything else would simply never hear.
	_ = stdin.Close()

	var err error

	select {
	case <-exit.done:
		err = exit.err
	case <-ctx.Done():
		// The connection goes rather than the process: the remote side sees
		// its stdin die either way, and this end must not hold a build open on
		// a machine that stopped answering.
		err = ctx.Err()
	}

	_ = client.Close()

	if err != nil && !errors.Is(err, io.EOF) {
		if note := diagnostics.String(); note != "" {
			return fmt.Errorf("the shim exited badly: %w (worker said: %s)", err, note)
		}

		return fmt.Errorf("the shim exited badly: %w", err)
	}

	return nil
}

func addressOf(worker Worker) string {
	_, _, err := net.SplitHostPort(worker.Host)
	if err == nil {
		return worker.Host
	}

	return net.JoinHostPort(worker.Host, defaultSSHPort)
}

// sshConfig assembles credentials and host-key verification for a worker.
func sshConfig(ctx context.Context, worker Worker) (*ssh.ClientConfig, error) {
	auths, err := authMethods(ctx, worker)
	if err != nil {
		return nil, err
	}

	hostKeys, err := hostKeyCallback(worker)
	if err != nil {
		return nil, err
	}

	user := worker.User
	if user == "" {
		user = os.Getenv("USER")
	}

	return &ssh.ClientConfig{
		User:            user,
		Auth:            auths,
		HostKeyCallback: hostKeys,
		Timeout:         dialTimeout,
	}, nil
}

// authMethods offers a named identity first, then whatever an agent holds.
//
// That order is deliberate and was a bug the other way round. An agent
// typically holds several keys and offers them all; when none is the worker's,
// the server can exhaust its MaxAuthTries before the identity the operator
// explicitly named is ever tried — so naming a key would fail on exactly the
// machines where naming one is the answer. Explicit beats ambient.
//
// The agent is still the common path, because it is how people actually
// authenticate and it keeps private keys where they already are rather than
// teaching this process to read them. SSH_AUTH_SOCK is deliberately absent
// from the allowlist a pipeline's own commands inherit — the socket signs with
// every key the operator holds, which is a credential capability rather than
// plumbing — but steps reading it to reach a worker is a different act, and
// the operator asked for it by naming the worker.
func authMethods(ctx context.Context, worker Worker) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	if worker.Identity != "" {
		method, err := keyFile(worker.Identity)
		if err != nil {
			return nil, err
		}

		methods = append(methods, method)
	}

	signers := agentSigners(ctx)
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if len(methods) == 0 {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, errNoAuth)
	}

	return methods, nil
}

// agentSigners asks the agent for its keys and hangs up.
//
// Eagerly, rather than handing the client a callback that reads the socket
// during authentication: a callback keeps the connection open for the life of
// the config, and a venue dialled per step would accumulate one agent
// connection — and the goroutine reading it — for every step that ever ran.
// The keys are the only thing wanted, and they do not change mid-handshake.
func agentSigners(ctx context.Context) []ssh.Signer {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		return nil
	}

	// The agent socket, named by the operator's own environment: the standard
	// way every ssh client on the machine reaches it, and a local unix socket
	// rather than anything a worker could influence.
	var dialer net.Dialer

	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()

	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		return nil
	}

	return signers
}

func keyFile(path string) (ssh.AuthMethod, error) {
	pem, err := os.ReadFile(path) //nolint:gosec // a key file the operator named on the worker URL, read on their behalf
	if err != nil {
		return nil, fmt.Errorf("reading the identity %q: %w", path, err)
	}

	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		// A passphrase-protected key is the common case here, and the useful
		// answer is the agent rather than a prompt this process would have to
		// grow a terminal to ask on.
		return nil, fmt.Errorf("parsing the identity %q (an encrypted key has to go through an agent): %w", path, err)
	}

	return ssh.PublicKeys(signer), nil
}

// hostKeyCallback verifies the worker is the machine it was last time.
//
// Never InsecureIgnoreHostKey. A feature whose entire job is to run commands
// on another machine cannot be the one that stops checking which machine that
// is; an operator who has not got a known_hosts entry yet is one ssh away from
// having one.
func hostKeyCallback(worker Worker) (ssh.HostKeyCallback, error) {
	file := worker.KnownHosts
	if file == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating known_hosts: %w", err)
		}

		file = filepath.Join(home, ".ssh", "known_hosts")
	}

	callback, err := knownhosts.New(file)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts %q (ssh to the worker once to record it, or point at one with ?known_hosts=): %w", file, err)
	}

	return callback, nil
}

// remoteShimPath is where a build of the binary lives on a worker.
func remoteShimPath(worker Worker, build string) string {
	root := worker.Root
	if root == "" {
		root = "/tmp"
	}

	return path.Join(root, "steps-shim", build, "steps")
}

// diagnosticBytes bounds how much of a worker's stderr is kept for an error
// message. Enough for a loader's complaint, not enough for a runaway.
const diagnosticBytes = 4 << 10

// diagnosticBuffer keeps the first few kilobytes a worker wrote outside the
// protocol, and drops the rest.
//
// Guarded, because the two ends of it genuinely race: the stderr copier writes
// whenever the worker says anything, and the handshake reads it the moment it
// decides the shim is not going to answer. That is precisely the moment a
// worker is most likely to be mid-complaint.
type diagnosticBuffer struct {
	mu   sync.Mutex
	buf  strings.Builder
	left int
}

func (d *diagnosticBuffer) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.left > 0 {
		chunk := p
		if len(chunk) > d.left {
			chunk = chunk[:d.left]
		}

		n, _ := d.buf.Write(chunk)
		d.left -= n
	}

	// Always the full length: a diagnostic that could not be stored must not
	// look like a short write to whatever is copying it.
	return len(p), nil
}

// String is what the worker said, trimmed.
func (d *diagnosticBuffer) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()

	return strings.TrimSpace(d.buf.String())
}
