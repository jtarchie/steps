package shell

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// startEngine answers _ping, hands every create to onCreate, fails every pull inside its stream, and 404s the rest.
func startEngine(t *testing.T, onCreate http.HandlerFunc) *httptest.Server {
	t.Helper()

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.44")
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			onCreate(w, r)
		case strings.HasSuffix(r.URL.Path, "/images/create"):
			_, _ = w.Write([]byte(`{"error":"repository steps-test/none not found"}` + "\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"No such image"}`))
		}
	}))
	t.Cleanup(engine.Close)

	return engine
}

func runAgainst(ctx context.Context, t *testing.T, host string) (Runner, int, error) {
	t.Helper()

	runner := newTestRunner(t, RunnerSpec{Image: "steps-test/none:1", DockerHost: host})
	_, _, code, err := RunCaptureFull(ctx, runner, "true")

	return runner, code, err
}

func refuse(status int, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"message":"` + message + `"}`))
	}
}

// A daemon that answers the create with no (unknown image, runtime refusal) is data: 125, the status `docker run` reports.
func TestDaemonRefusalOfTheStartIsAnExitError125(t *testing.T) {
	t.Parallel()

	for name, handler := range map[string]http.HandlerFunc{
		"unknown image": refuse(http.StatusNotFound, "No such image"),
		"runtime error": refuse(http.StatusInternalServerError, "OCI runtime create failed"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			engine := startEngine(t, handler)

			_, code, err := runAgainst(t.Context(), t, "tcp://"+engine.Listener.Addr().String())
			if err != nil || code != dockerDaemonRefusedCode {
				t.Fatalf("code = %d, err = %v; want exit status %d as data", code, err, dockerDaemonRefusedCode)
			}
		})
	}
}

// A daemon nobody can reach never said anything about the image, so it is not the command saying no.
func TestUnreachableDaemonIsNotAnExitError(t *testing.T) {
	t.Parallel()

	engine := startEngine(t, refuse(http.StatusInternalServerError, "unused"))
	host := "tcp://" + engine.Listener.Addr().String()
	engine.Close()

	_, code, err := runAgainst(t.Context(), t, host)
	if err == nil || code == dockerDaemonRefusedCode {
		t.Fatalf("code = %d, err = %v; want a plain infrastructure error", code, err)
	}
}

// The create is detached from the caller (dockerapi.createContainer), so the cancel lands while the daemon is still answering. A cancel mid-create is the caller leaving, not the image being refused: no ExitError (a guard would read it as the answer no), and the failure must not pin the session so a later call retries.
func TestCancelledStartIsInfrastructureAndNotSticky(t *testing.T) {
	t.Parallel()

	var creates atomic.Int32

	entered := make(chan struct{}, 1)
	released := make(chan struct{})

	engine := startEngine(t, func(w http.ResponseWriter, r *http.Request) {
		if creates.Add(1) > 1 {
			refuse(http.StatusInternalServerError, "second attempt reached the daemon")(w, r)

			return
		}

		entered <- struct{}{}

		<-released

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"Id":"gone","Warnings":[]}`))
	})

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		<-entered
		cancel()
		close(released)
	}()

	runner, code, err := runAgainst(ctx, t, "tcp://"+engine.Listener.Addr().String())
	if err == nil || code == dockerDaemonRefusedCode {
		t.Fatalf("cancelled start: code = %d, err = %v; want a plain infrastructure error", code, err)
	}

	_, _, _, err = RunCaptureFull(t.Context(), runner, "true")
	if got := creates.Load(); got != 2 {
		t.Fatalf("creates = %d (second call err %v), want 2: the cancelled start was pinned for the session", got, err)
	}
}
