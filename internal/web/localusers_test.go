package web

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestAnHtpasswdFileSkipsCommentsAndBlankLines(t *testing.T) {
	t.Parallel()

	hash := mustHash(t, "pw")

	users, err := ParseLocalUsers(strings.NewReader("# operators\n\nalex:" + hash + "\n  sam:" + hash + "  \n"))
	if err != nil || len(users) != 2 || users[0].Name != "alex" || users[1].Name != "sam" {
		t.Fatalf("ParseLocalUsers = %+v, %v; want alex and sam", users, err)
	}
}

func TestAPlainPasswordIsRefusedWithoutBeingRepeated(t *testing.T) {
	t.Parallel()

	_, err := ParseLocalUsers(strings.NewReader("alex:" + mustHash(t, "pw") + "\nsam:hunter2\n"))
	if !errors.Is(err, ErrPlainPassword) || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("err = %v; want ErrPlainPassword naming line 2 and not the password", err)
	}

	for _, entry := range []string{"no-colon", ":" + mustHash(t, "pw")} {
		_, err := ParseLocalUser(entry)
		if err == nil {
			t.Errorf("ParseLocalUser(%q) accepted an entry with no name", entry)
		}
	}
}

func TestAVerifiedPairSkipsBcryptUntilItExpires(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	auth := &basicAuth{users: map[string][]byte{"alex": []byte(mustHash(t, "pw"))}}
	auth.seen.now = func() time.Time { return now }

	if auth.matches("alex", "wrong") || !auth.matches("alex", "pw") {
		t.Fatal("bcrypt must decide the first check")
	}

	// The hash goes; only the cache can still answer.
	auth.users["alex"] = []byte(mustHash(t, "changed"))

	if !auth.matches("alex", "pw") {
		t.Error("a pair verified a moment ago was checked again")
	}

	now = now.Add(verifiedTTL + time.Second)

	if auth.matches("alex", "pw") {
		t.Error("a verified pair outlived verifiedTTL")
	}
}

func TestAnUnknownNameIsRefused(t *testing.T) {
	t.Parallel()

	auth := &basicAuth{users: map[string][]byte{"alex": []byte(mustHash(t, "pw"))}}
	if auth.matches("nobody", "pw") {
		t.Error("an unknown name was accepted")
	}
}

func mustHash(t *testing.T, password string) string {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}

	return string(hash)
}
