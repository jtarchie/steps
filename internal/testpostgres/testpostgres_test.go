package testpostgres

import (
	"errors"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

func fakeStart(t *testing.T, errs ...error) *int {
	t.Helper()

	calls := 0
	orig := startDB
	startDB = func(*embeddedpostgres.EmbeddedPostgres) error {
		defer func() { calls++ }()

		if calls < len(errs) {
			return errs[calls]
		}

		return nil
	}

	t.Cleanup(func() { startDB = orig })

	return &calls
}

var errFetch = errors.New("error fetching postgres: unable to connect to repo1.maven.org")

func TestUnavailableSkipsByDefault(t *testing.T) {
	t.Setenv(RequireEnv, "")
	fakeStart(t, errFetch)

	_, err := Start(nil)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable so callers skip", err)
	}
}

func TestUnavailableFailsWhenRequired(t *testing.T) {
	t.Setenv(RequireEnv, "1")
	fakeStart(t, errFetch)

	_, err := Start(nil)
	if err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want a failure that is not ErrUnavailable", err)
	}
}

func TestTakenPortIsRetried(t *testing.T) {
	taken := errors.New("listen tcp: address already in use")
	calls := fakeStart(t, taken, taken)

	server, err := Start(nil)
	if err != nil || server == nil {
		t.Fatalf("Start = %v, %v; want success on the third attempt", server, err)
	}

	if *calls != 3 {
		t.Errorf("startDB calls = %d, want 3", *calls)
	}
}

func TestTakenPortGivesUpAfterThree(t *testing.T) {
	taken := errors.New("listen tcp: address already in use")
	calls := fakeStart(t, taken, taken, taken, taken)

	_, err := Start(nil)
	if err == nil || *calls != 3 {
		t.Fatalf("err = %v after %d calls, want an error after exactly 3", err, *calls)
	}
}
