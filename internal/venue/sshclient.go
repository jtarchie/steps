package venue

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// sshHandshake runs the SSH handshake with a watchdog. The tunnel conn has
// no deadlines (its SetDeadline is a no-op) and NewClientConn ignores
// config.Timeout — only ssh.Dial's TCP dial honors it — so a peer that
// stalls mid-handshake would otherwise hold the dial forever, and closing
// the conn is the one interruption that reaches a blocked handshake.
func sshHandshake(conn net.Conn, address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	watchdog := time.AfterFunc(config.Timeout, func() { _ = conn.Close() })
	defer watchdog.Stop()

	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		_ = conn.Close()

		return nil, err //nolint:wrapcheck // the caller names the worker and instance
	}

	// Stop answers false once the watchdog has already fired, which means the
	// conn this client would ride on is being closed right now: report the
	// timeout the watchdog exists to report, rather than hand back a live
	// client over a dead transport whose first use fails as something else.
	if !watchdog.Stop() {
		_ = sshConn.Close()

		return nil, fmt.Errorf("the SSH handshake did not finish within %s", config.Timeout)
	}

	return ssh.NewClient(sshConn, channels, requests), nil
}

// hostKeyAlgorithms are the signature algorithms a pinned key can verify; an RSA key signs as rsa-sha2-*, never as its own type name.
func hostKeyAlgorithms(key ssh.PublicKey) []string {
	if key.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}

	return []string{key.Type()}
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

// errHostKeyMismatch is a worker that is not the machine it was pinned to —
// by a hostkey= pin, by the key its install reported, or by the keys its
// guest attributes attest.
var errHostKeyMismatch = errors.New("the worker's host key is not the one it was pinned to")

// fixedHostKey is ssh.FixedHostKey with a mismatch classified, so a caller
// asks errors.Is rather than matching upstream's message.
func fixedHostKey(want ssh.PublicKey) ssh.HostKeyCallback {
	fixed := ssh.FixedHostKey(want)

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := fixed(hostname, remote, key)
		if err != nil && want != nil {
			return fmt.Errorf("%w: %s offered %s, expected %s: %w",
				errHostKeyMismatch, remote, ssh.FingerprintSHA256(key), ssh.FingerprintSHA256(want), err)
		}

		return err
	}
}

// The ephemeral identity for workers this process installs a key on: one
// keypair per process, made once and never written to disk.
//
//nolint:gochecknoglobals // process-lifetime identity, generated once
var (
	sshKeyOnce   sync.Once
	sshKeySigner ssh.Signer
	sshKeyErr    error
)

// sshIdentity is this process's SSH identity for workers it installs a key on (gcp://, aws://).
func sshIdentity() (ssh.Signer, error) {
	sshKeyOnce.Do(func() {
		_, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			sshKeyErr = fmt.Errorf("generating this process's SSH key for workers: %w", err)

			return
		}

		signer, err := ssh.NewSignerFromKey(private)
		if err != nil {
			sshKeyErr = fmt.Errorf("wrapping this process's SSH key for workers: %w", err)

			return
		}

		sshKeySigner = signer
	})

	if sshKeyErr != nil {
		return nil, sshKeyErr
	}

	if sshKeySigner == nil {
		// Unreachable while the Once above sets one of the two — stated
		// locally so a caller's nil check never depends on inferring that.
		return nil, errors.New("no SSH key was generated for workers")
	}

	return sshKeySigner, nil
}
