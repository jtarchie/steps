package shell

// A faked engine, for paths a real daemon will not take on request (a missing image pullable with no network, a refused removal); it encodes this package's reading of the API, while dockercontract_test.go asks the real daemon.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/events"
)

func fakeEngine(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) *dockerapi.Client {
	t.Helper()

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.Header().Set("Api-Version", "1.44")

			return
		}

		handle(w, r)
	}))
	t.Cleanup(engine.Close)

	client, err := dockerapi.New("tcp://" + engine.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dockerapi.New: %v", err)
	}

	t.Cleanup(func() { _ = client.Close() })

	return client
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	err := json.NewEncoder(w).Encode(body)
	if err != nil {
		t.Errorf("encode fake engine response: %v", err)
	}
}

// captureLogs swaps the default logger for one this test reads. Not safe beside a parallel test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	var logs bytes.Buffer

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	return &logs
}

// A placed step's image is pulled at the moment its container is created, because the worker had no daemon to pull onto any earlier.
func TestSessionPullsAMissingImageAndCreatesAgain(t *testing.T) {
	t.Parallel()

	var creates, pulls atomic.Int32

	client := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			if creates.Add(1) == 1 {
				writeJSONResponse(t, w, http.StatusNotFound, map[string]string{"message": "No such image: steps-test/missing:1"})

				return
			}

			writeJSONResponse(t, w, http.StatusCreated, map[string]any{"Id": "created-after-pull", "Warnings": []string{}})
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			pulls.Add(1)
			writeJSONResponse(t, w, http.StatusOK, map[string]string{"status": "Downloaded newer image"})
		case strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected engine call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})

	session := &dockerSession{image: "steps-test/missing:1", client: client}

	var progress bytes.Buffer

	ctx := events.WithOutput(t.Context(), events.Output{Stdout: &progress, Stderr: &progress})

	id, err := session.start(ctx, "steps-test-pull")
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	if id != "created-after-pull" {
		t.Errorf("id = %q, want the container created after the pull", id)
	}

	if pulls.Load() != 1 || creates.Load() != 2 {
		t.Errorf("pulls = %d, creates = %d; want one pull between two creates", pulls.Load(), creates.Load())
	}
}

// A container that could not be taken away is a leak; the warning is the only trace of it.
func TestReclaimWarnsWhenTheDaemonRefusesTheRemoval(t *testing.T) {
	logs := captureLogs(t)

	client := fakeEngine(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(t, w, http.StatusInternalServerError, map[string]string{"message": "driver failed"})
	})

	reclaim(t.Context(), client, "stuck")

	if !strings.Contains(logs.String(), "shell.docker.remove_failed") || !strings.Contains(logs.String(), "stuck") {
		t.Errorf("logs = %q, want a removal failure naming the container", logs.String())
	}
}

func TestSweepWarnsOnlyAboutARemovalThatFailed(t *testing.T) {
	const deadPID = "4194303"

	for _, tc := range []struct {
		name   string
		status int
		warned bool
	}{
		{name: "refused", status: http.StatusInternalServerError, warned: true},
		{name: "removed", status: http.StatusNoContent, warned: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)

			client := fakeEngine(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					writeJSONResponse(t, w, http.StatusOK, []map[string]any{{
						"Id":     "orphan",
						"Labels": map[string]string{dockerOwnerLabel: "steps", dockerHostLabel: ownerHostname(), dockerPIDLabel: deadPID},
					}})
				case http.MethodDelete:
					if tc.status == http.StatusNoContent {
						w.WriteHeader(tc.status)

						return
					}

					writeJSONResponse(t, w, tc.status, map[string]string{"message": "driver failed"})
				}
			})

			SweepOrphanedContainersOn(t.Context(), client)

			if !strings.Contains(logs.String(), "shell.docker.sweep_orphan") {
				t.Fatalf("logs = %q, want the orphan swept", logs.String())
			}

			if got := strings.Contains(logs.String(), "shell.docker.sweep_failed"); got != tc.warned {
				t.Errorf("sweep_failed logged = %v, want %v: %s", got, tc.warned, logs.String())
			}
		})
	}
}

// A daemon that never answers a removal must not hold the session's lock forever: ensure reclaims under it, and close takes the same lock.
func TestAHungRemovalDoesNotWedgeTheSession(t *testing.T) {
	previous := dockerCleanupTimeout
	dockerCleanupTimeout = 100 * time.Millisecond

	t.Cleanup(func() { dockerCleanupTimeout = previous })

	engine := hungRemovalEngine(t)

	session := &dockerSession{image: "steps-test/hung:1", dockerHost: "tcp://" + engine.Listener.Addr().String()}

	done := make(chan error, 1)

	go func() {
		_, _, _, err := session.ensure(t.Context())
		_ = session.close()

		done <- err
	}()

	select {
	case err := <-done:
		if !IsExitError(err) {
			t.Errorf("ensure = %v, want the daemon's refusal as an exit error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ensure/close still blocked on a removal the daemon never answered")
	}
}

// hungRemovalEngine creates a container, refuses to start it, and never answers the removal that follows until the client gives up or the test ends.
func hungRemovalEngine(t *testing.T) *httptest.Server {
	t.Helper()

	release := make(chan struct{})

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.44")
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			writeJSONResponse(t, w, http.StatusCreated, map[string]any{"Id": "never-started", "Warnings": []string{}})
		case r.Method == http.MethodDelete:
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			writeJSONResponse(t, w, http.StatusInternalServerError, map[string]string{"message": "cannot start"})
		}
	}))
	// Close waits on every request in flight, so the hung removal is released first.
	t.Cleanup(engine.Close)
	t.Cleanup(func() { close(release) })

	return engine
}
