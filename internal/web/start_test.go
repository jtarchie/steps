package web

// What `steps web` actually listens with.

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestTheListenerKeepsItsOwnReadTimeouts: echo builds the http.Server and no longer hands it over as a field, so every timeout this daemon cares about now lives in one callback — and both of them are load-bearing in a way no page test can see.
func TestTheListenerKeepsItsOwnReadTimeouts(t *testing.T) {
	t.Parallel()

	config := startConfig("127.0.0.1:0")

	if config.BeforeServeFunc == nil {
		t.Fatal("nothing reaches the http.Server, so it keeps echo's defaults")
	}

	server := &http.Server{} //nolint:gosec // G112: the callback under test is what sets the timeout

	err := config.BeforeServeFunc(server)
	if err != nil {
		t.Fatalf("BeforeServeFunc: %v", err)
	}

	if server.ReadHeaderTimeout != readHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v — a sender dribbling headers holds a connection", server.ReadHeaderTimeout, readHeaderTimeout)
	}

	// echo's own default is 30s, which bounds the WHOLE request: net/http's background read on a bodyless GET hits that deadline while the handler still runs and cancels the request context, dropping every live stream every 30 seconds and costing the reader their fold on each reconnect.
	if server.ReadTimeout != 0 {
		t.Errorf("ReadTimeout = %v, want 0 — a live stream outlives any whole-request deadline", server.ReadTimeout)
	}

	if config.GracefulTimeout != shutdownGrace {
		t.Errorf("GracefulTimeout = %v, want %v", config.GracefulTimeout, shutdownGrace)
	}

	if !config.HideBanner || !config.HidePort {
		t.Error("the daemon prints its own address; echo's banner and port lines are noise on top of it")
	}
}

// TestStartServesUntilItsContextIsCanceled: serving and shutting down are echo's own now rather than this package's goroutine, and a cancel that does not return is a `steps web` that will not exit.
func TestStartServesUntilItsContextIsCanceled(t *testing.T) {
	t.Parallel()

	server, _ := managedServer(t)

	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(t.Context())

	stopped := make(chan error, 1)
	go func() { stopped <- server.Start(ctx, addr) }()

	body := waitForServe(t, addr)
	if body == "" {
		t.Fatal("the server answered nothing on its own address")
	}

	cancel()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Start after a cancel = %v, want nil", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("Start did not return after its context was canceled")
	}
}

// freeAddr is a loopback address nothing is listening on. Bound and released rather than picked, because a hardcoded port is the one way this test can fail on a machine that is simply busy.
func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	addr := listener.Addr().String()

	err = listener.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	return addr
}

// waitForServe polls the root until the listener is up, so the test does not race the goroutine that started it.
func waitForServe(t *testing.T, addr string) string {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if readErr != nil {
			t.Fatalf("reading the body: %v", readErr)
		}

		return string(body)
	}

	t.Fatal("the server never came up")

	return ""
}
