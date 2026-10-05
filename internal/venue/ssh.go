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
	"net"
	"os"
	"path/filepath"
	"strings"
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
)

// sshClientFor authenticates to worker's sshd, host key checked, for any scheme whose transport is ssh.
func sshClientFor(ctx context.Context, worker Worker) (*ssh.Client, error) {
	// Resolution first, and once: an alias out of the operator's ssh_config
	// is not a hostname, so everything below -- the address, the credentials,
	// the file host keys are checked against -- is downstream of it.
	settings, err := connectionFor(worker)
	if err != nil {
		return nil, err
	}

	config, err := sshConfig(ctx, settings)
	if err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: dialTimeout}

	conn, err := dialer.DialContext(ctx, "tcp", settings.address)
	if err != nil {
		return nil, fmt.Errorf("dialing: %w", err)
	}

	sshConn, channels, requests, err := ssh.NewClientConn(conn, settings.address, config)
	if err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("connecting: %w", err)
	}

	return ssh.NewClient(sshConn, channels, requests), nil
}

// sshConfig assembles credentials and host-key verification for a worker.
func sshConfig(ctx context.Context, settings connection) (*ssh.ClientConfig, error) {
	auths, err := authMethods(ctx, settings)
	if err != nil {
		return nil, err
	}

	hostKeys, err := hostKeyCallback(settings)
	if err != nil {
		return nil, err
	}

	return &ssh.ClientConfig{
		User:            settings.user,
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
func authMethods(ctx context.Context, settings connection) ([]ssh.AuthMethod, error) {
	var (
		methods []ssh.AuthMethod
		skipped []string
	)

	for _, candidate := range settings.identities {
		method, err := keyFile(candidate.path)
		if err != nil {
			// A key the operator named is an answer they already gave, so a
			// bad one is an error. One a config file named is a candidate --
			// a Host * block routinely names a key that is absent here or
			// encrypted, and OpenSSH just moves on to the next. Remembered
			// rather than dropped, so an end with nothing to offer can say
			// which candidates it looked at and why each was passed over.
			if candidate.explicit {
				return nil, err
			}

			skipped = append(skipped, fmt.Sprintf("%s (%v)", candidate.path, err))

			continue
		}

		methods = append(methods, method)
	}

	signers := agentSigners(ctx)
	if len(signers) > 0 {
		methods = append(methods, ssh.PublicKeys(signers...))
	}

	if len(methods) == 0 {
		if len(skipped) > 0 {
			return nil, fmt.Errorf("worker %q: %w — the ssh_config named %s, skipped as ssh would skip it",
				settings.worker.URL, errNoAuth, strings.Join(skipped, "; "))
		}

		return nil, fmt.Errorf("worker %q: %w", settings.worker.URL, errNoAuth)
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
func hostKeyCallback(settings connection) (ssh.HostKeyCallback, error) {
	if settings.hostKey != "" {
		return pinnedHostKey(settings.hostKey), nil
	}

	files := settings.knownHosts

	switch {
	case settings.ambientHosts:
		// A per-host file the operator's ssh_config names may simply not exist
		// yet, and OpenSSH reads an absent known_hosts as empty -- the host is
		// then unknown, which is already a refusal. Falling back to
		// ~/.ssh/known_hosts is NOT that answer: it would check against a file
		// the operator's own config excluded.
		files = existingFiles(files)
	case len(files) == 0:
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating known_hosts: %w", err)
		}

		files = []string{filepath.Join(home, ".ssh", "known_hosts")}
	}

	callback, err := knownhosts.New(files...)
	if err != nil {
		return nil, fmt.Errorf("reading known_hosts %q (ssh to the worker once to record it, or point at one with ?known_hosts=): %w", strings.Join(files, ", "), err)
	}

	return callback, nil
}

// existingFiles drops the names that are not there.
func existingFiles(paths []string) []string {
	kept := make([]string, 0, len(paths))

	for _, path := range paths {
		_, err := os.Stat(path)
		if err == nil {
			kept = append(kept, path)
		}
	}

	return kept
}

// pinnedHostKey verifies a worker against a fingerprint the operator supplied.
//
// The mismatch names BOTH fingerprints. A pin that failed saying only "host
// key mismatch" leaves an operator unable to tell a replaced machine from a
// mistyped mapping, and those two want opposite responses.
func pinnedHostKey(want string) ssh.HostKeyCallback {
	return func(_ string, remote net.Addr, key ssh.PublicKey) error {
		got := ssh.FingerprintSHA256(key)
		if got != want {
			return fmt.Errorf("%w: %s offered %s and the mapping pins %s",
				errHostKeyMismatch, remote, got, want)
		}

		return nil
	}
}

// errHostKeyMismatch is a worker that is not the machine it was pinned to.
var errHostKeyMismatch = errors.New("the worker's host key does not match its hostkey= pin")

// shellQuote wraps a path for the remote login shell.
//
// An SSH exec request is a string the far end hands to a shell, so an
// unquoted path with a space in it becomes two arguments and one with a
// semicolon becomes two commands. The path is built from the worker URL, which
// only an operator writes -- but a mapping naming a disk with a space in its
// name should mount that disk, not fail obscurely.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
