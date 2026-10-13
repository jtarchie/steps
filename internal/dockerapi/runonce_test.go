package dockerapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// RunOnce's removal is detached from its caller, so a daemon that never answers it must still let RunOnce return.
func TestRunOnceReturnsWhenTheRemovalHangs(t *testing.T) {
	previous := removalBound
	removalBound = 100 * time.Millisecond

	t.Cleanup(func() { removalBound = previous })

	engine := hungRemovalEngine(t)

	client, err := New("tcp://" + engine.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	done := make(chan error, 1)

	go func() {
		_, _, _, runErr := client.RunOnce(t.Context(), ContainerSpec{Image: "steps-test/hung:1"})
		done <- runErr
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("RunOnce succeeded against a daemon that refused the start")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunOnce still blocked on a removal the daemon never answered")
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
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"never-started","Warnings":[]}`))
		case r.Method == http.MethodDelete:
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"cannot start"}`))
		}
	}))
	// Close waits on every request in flight, so the hung removal is released first.
	t.Cleanup(engine.Close)
	t.Cleanup(func() { close(release) })

	return engine
}
