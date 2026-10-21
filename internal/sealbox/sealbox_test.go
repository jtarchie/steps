package sealbox_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/jtarchie/steps/internal/sealbox"
)

func newKey(t *testing.T) string {
	t.Helper()

	key := make([]byte, 32)
	_, _ = rand.Read(key)

	return base64.StdEncoding.EncodeToString(key)
}

func box(t *testing.T, env map[string]string) *sealbox.Box {
	t.Helper()

	b, err := sealbox.FromEnv(func(name string) string { return env[name] })
	if err != nil {
		t.Fatal(err)
	}

	return b
}

func TestSealOpensOnlyUnderItsKeyAndAAD(t *testing.T) {
	t.Parallel()

	key := newKey(t)
	b := box(t, map[string]string{sealbox.KeyEnv: key})
	secret := []byte("refresh-token-value")

	sealed, err := b.Seal(secret, []byte("1|mcp:tracker"))
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(sealed, secret) {
		t.Fatal("the sealed value carries the plaintext")
	}

	plain, stale, err := b.Open(sealed, []byte("1|mcp:tracker"))
	if err != nil || stale || !bytes.Equal(plain, secret) {
		t.Fatalf("Open = %q, stale=%v, %v", plain, stale, err)
	}

	_, _, err = b.Open(sealed, []byte("2|mcp:tracker"))
	if !errors.Is(err, sealbox.ErrWrongKey) {
		t.Errorf("Open under another pipeline's aad = %v, want ErrWrongKey", err)
	}

	other := box(t, map[string]string{sealbox.KeyEnv: newKey(t)})

	_, _, err = other.Open(sealed, []byte("1|mcp:tracker"))
	if !errors.Is(err, sealbox.ErrWrongKey) {
		t.Errorf("Open under another key = %v, want ErrWrongKey", err)
	}
}

func TestNoKeyRefusesToSeal(t *testing.T) {
	t.Parallel()

	_, err := box(t, map[string]string{}).Seal([]byte("x"), nil)
	if !errors.Is(err, sealbox.ErrNoKey) {
		t.Errorf("Seal with no key = %v, want ErrNoKey", err)
	}
}

func TestOldKeyOpensAndReportsStale(t *testing.T) {
	t.Parallel()

	old := newKey(t)

	sealed, err := box(t, map[string]string{sealbox.KeyEnv: old}).Seal([]byte("x"), []byte("a"))
	if err != nil {
		t.Fatal(err)
	}

	rotated := box(t, map[string]string{sealbox.KeyEnv: newKey(t), sealbox.OldKeyEnv: old})

	plain, stale, err := rotated.Open(sealed, []byte("a"))
	if err != nil || !stale || string(plain) != "x" {
		t.Fatalf("Open during rotation = %q, stale=%v, %v; want x, stale", plain, stale, err)
	}
}

func TestMalformedKeysAreRefusedUpFront(t *testing.T) {
	t.Parallel()

	short := base64.StdEncoding.EncodeToString([]byte("too-short"))

	for name, env := range map[string]map[string]string{
		"not base64":        {sealbox.KeyEnv: "!!!"},
		"wrong length":      {sealbox.KeyEnv: short},
		"old without new":   {sealbox.OldKeyEnv: newKey(t)},
		"malformed old key": {sealbox.KeyEnv: newKey(t), sealbox.OldKeyEnv: short},
	} {
		_, err := sealbox.FromEnv(func(n string) string { return env[n] })
		if err == nil {
			t.Errorf("%s: FromEnv accepted it", name)
		}
	}
}
