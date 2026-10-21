package dockerapi

import (
	"encoding/binary"
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

// An image chooses what it prints; RunOnce must not hold all of it.
func TestRunOnceCapsAnOversizeOutput(t *testing.T) {
	t.Parallel()

	engine := oversizeLogsEngine(t)

	client, err := New("tcp://" + engine.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = client.Close() })

	code, stdout, _, err := client.RunOnce(t.Context(), ContainerSpec{Image: "steps-test/big:1"})
	if err != nil || code != 0 {
		t.Fatalf("RunOnce = %d, %v; want the oversize run to succeed", code, err)
	}

	if !strings.HasPrefix(stdout, "first line\n") || !strings.HasSuffix(stdout, "[output truncated at 1 MiB]") {
		t.Errorf("stdout lost its head or its marker: %.20q ... %q", stdout, stdout[max(0, len(stdout)-40):])
	}

	if len(stdout) > runOnceOutputCap+64 {
		t.Errorf("stdout = %d bytes, want it capped near %d", len(stdout), runOnceOutputCap)
	}
}

// oversizeLogsEngine answers a create/start/wait/logs round with three MiB of stdout after a short first line.
func oversizeLogsEngine(t *testing.T) *httptest.Server {
	t.Helper()

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.Header().Set("Api-Version", "1.44")
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"Id":"big","Warnings":[]}`))
		case strings.HasSuffix(r.URL.Path, "/start"), r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/wait"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"StatusCode":0}`))
		case strings.HasSuffix(r.URL.Path, "/logs"):
			frame := func(p []byte) {
				header := make([]byte, 8)
				header[0] = 1
				binary.BigEndian.PutUint32(header[4:], uint32(len(p))) //nolint:gosec // test frames are a few MiB
				_, _ = w.Write(header)
				_, _ = w.Write(p)
			}

			frame([]byte("first line\n"))

			for range 3 {
				frame(make([]byte, 1<<20))
			}
		}
	}))
	t.Cleanup(engine.Close)

	return engine
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
