package venue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/shell"
)

// fakeIMDS answers like EC2's metadata service: a token for a PUT, and no spot notice until noticed is set.
func fakeIMDS(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()

	var noticed atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			_, _ = w.Write([]byte("token"))
		case r.URL.Path == "/latest/meta-data/spot/instance-action" && noticed.Load():
			_, _ = w.Write([]byte(`{"action":"terminate","time":"2026-10-04T20:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	previousBase, previousPoll := awsMetadataBase, drainPoll
	awsMetadataBase, drainPoll = server.URL, 0

	t.Cleanup(func() { awsMetadataBase, drainPoll = previousBase, previousPoll })

	return server, &noticed
}

// The two-minute warning reaches the pipeline: ReclaimedBy says so, and a command the reclamation killed is an eviction, re-placed without spending the step's attempts, while one that answered for itself is not.
func TestAWSWorkerHearsItsOwnSpotNotice(t *testing.T) {
	_, noticed := fakeIMDS(t)

	runner := newLocalRunner(t, localSSMWorker(t, &fakeSSM{}, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}

	if _, reclaimed := ReclaimedBy(runner); reclaimed {
		t.Fatal("reclaimed before any notice")
	}

	noticed.Store(true)

	deadline := time.Now().Add(10 * time.Second)
	for {
		reason, reclaimed := ReclaimedBy(runner)
		if reclaimed {
			if reason == "" {
				t.Error("reclaimed with no reason given")
			}

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the spot notice never reached the runner")
		}

		time.Sleep(100 * time.Millisecond)
	}

	err = runner.Run(context.Background(), "kill -9 $$")
	if !errors.Is(err, ErrEvicted) {
		t.Errorf("a command the machine killed: %v, want ErrEvicted", err)
	}

	err = runner.Run(context.Background(), "exit 3")
	if errors.Is(err, ErrEvicted) || !shell.IsExitError(err) {
		t.Errorf("a command that answered for itself: %v, want its own exit", err)
	}
}

// sshd signals nothing to a non-pty command when its client goes; the watcher must end with its session or it polls the metadata service until the machine reboots.
func TestAWSDrainWatcherEndsWithItsSession(t *testing.T) {
	server, _ := fakeIMDS(t)

	runner := newLocalRunner(t, localSSMWorker(t, &fakeSSM{}, t.TempDir()))

	err := runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatal(err)
	}

	// Running first, or its absence afterwards proves nothing.
	if exec.CommandContext(t.Context(), "pgrep", "-f", server.URL).Run() != nil { //nolint:gosec // a URL this test minted
		t.Fatal("no drain watcher was running while the session was open")
	}

	err = runner.Close()
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for exec.CommandContext(t.Context(), "pgrep", "-f", server.URL).Run() == nil { //nolint:gosec // a URL this test minted
		if time.Now().After(deadline) {
			t.Fatal("the drain watcher outlived its session")
		}

		time.Sleep(100 * time.Millisecond)
	}
}
