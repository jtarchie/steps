// Package sealbox encrypts the secrets steps keeps in its state database, so a copy of the database without the key is worth nothing. Stdlib only.
package sealbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeyEnv names the key every sealed value is encrypted under; OldKeyEnv the one it replaces during a rotation.
const (
	KeyEnv    = "STEPS_ENCRYPTION_KEY"
	OldKeyEnv = "STEPS_OLD_ENCRYPTION_KEY"
)

// version is the first byte of every sealed value, so a later format can be told apart from this one.
const version byte = 1

// ErrNoKey is a seal asked of a box with no key: steps refuses to store a secret it cannot encrypt rather than store it in the clear.
var ErrNoKey = fmt.Errorf("no encryption key: set %s to a base64-encoded 32-byte key (openssl rand -base64 32) to store credentials in the state database", KeyEnv)

// ErrWrongKey is a sealed value no configured key opens: tampered, moved between rows, or sealed under a key this process was not given.
var ErrWrongKey = fmt.Errorf("no configured key opens this credential: it was sealed under another key (set %s to the old one to rotate) or the row was altered", OldKeyEnv)

// Box seals under the current key and opens under the current or the old one.
type Box struct {
	current cipher.AEAD
	old     cipher.AEAD
}

// FromEnv builds a Box from the two variables' values. Both empty is a Box that opens nothing and refuses to seal; a value that is not a 32-byte key is an error now rather than at the first login.
func FromEnv(lookup func(string) string) (*Box, error) {
	current, err := parse(KeyEnv, lookup(KeyEnv))
	if err != nil {
		return nil, err
	}

	old, err := parse(OldKeyEnv, lookup(OldKeyEnv))
	if err != nil {
		return nil, err
	}

	if current == nil && old != nil {
		return nil, fmt.Errorf("%s is set without %s: a rotation needs the key it rotates to", OldKeyEnv, KeyEnv)
	}

	return &Box{current: current, old: old}, nil
}

func parse(name, value string) (cipher.AEAD, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil //nolint:nilnil // an unset key is a legitimate state, which Seal refuses and Open reports
	}

	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key (openssl rand -base64 32)", name)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return aead, nil
}

// Ready is ErrNoKey when there is no key to seal under, so a caller can refuse before it has written anything else.
func (b *Box) Ready() error {
	if b == nil || b.current == nil {
		return ErrNoKey
	}

	return nil
}

// Seal encrypts plain under the current key, binding it to aad: a value opened with any other aad fails, which is what stops a row being copied onto another pipeline's.
func (b *Box) Seal(plain, aad []byte) ([]byte, error) {
	err := b.Ready()
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, b.current.NonceSize())

	_, err = rand.Read(nonce)
	if err != nil {
		return nil, fmt.Errorf("sealbox: nonce: %w", err)
	}

	out := append([]byte{version}, nonce...)

	return b.current.Seal(out, nonce, plain, aad), nil
}

// Open decrypts sealed. stale reports that only the old key opened it, so the caller re-seals it under the current one.
func (b *Box) Open(sealed, aad []byte) (plain []byte, stale bool, err error) {
	if b == nil || b.current == nil {
		return nil, false, ErrNoKey
	}

	if len(sealed) < 1 || sealed[0] != version {
		return nil, false, errors.New("sealbox: not a sealed value this version of steps reads")
	}

	plain, err = open(b.current, sealed[1:], aad)
	if err == nil {
		return plain, false, nil
	}

	if b.old != nil {
		plain, err = open(b.old, sealed[1:], aad)
		if err == nil {
			return plain, true, nil
		}
	}

	return nil, false, ErrWrongKey
}

func open(aead cipher.AEAD, body, aad []byte) ([]byte, error) {
	if len(body) < aead.NonceSize() {
		return nil, errors.New("sealbox: truncated")
	}

	nonce, ciphertext := body[:aead.NonceSize()], body[aead.NonceSize():]

	return aead.Open(nil, nonce, ciphertext, aad) //nolint:wrapcheck // the caller maps every failure to ErrWrongKey
}
