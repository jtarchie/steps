package web

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// liveRunServer is a signed-in daemon with a run that never finishes and one event for a stream to deliver.
func liveRunServer(t *testing.T) (*Server, store.Accounts, store.Session) {
	t.Helper()

	_, pipeline := testPipeline(t)

	accounts, err := sqlite.OpenAccounts(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenAccounts: %v", err)
	}

	t.Cleanup(func() { _ = accounts.Close() })

	server, err := New([]*Pipeline{pipeline}, nil, WithSessions(accounts), WithLocalUsers([]LocalUser{{Name: "alex", Hash: []byte(mustHash(t, "a"))}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	session := sessionFor("alex")

	err = accounts.CreateSession(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}

	err = pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "run-live", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-live", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
	})

	return server, accounts, session
}

// openLiveStream returns the stream once its first frame has arrived.
func openLiveStream(t *testing.T, server *Server) *bufio.Reader {
	t.Helper()

	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, front.URL+"/p/demo/runs/run-live/events", nil)
	if err != nil {
		t.Fatal(err)
	}

	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "alex", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("opening the stream: %v", err)
	}

	t.Cleanup(func() { _ = resp.Body.Close() })

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream closed before it delivered anything: %v", err)
		}

		if strings.HasPrefix(line, "data:") {
			return reader
		}
	}
}

// A stream is opened once and runs as long as the job does, so the sign-out that happens in the middle is the case. Serial, for the interval it shrinks.
func TestARevokedSessionEndsItsLiveStream(t *testing.T) {
	previous := liveRecheckInterval
	liveRecheckInterval = 0

	t.Cleanup(func() { liveRecheckInterval = previous })

	server, accounts, session := liveRunServer(t)
	reader := openLiveStream(t, server)

	err := accounts.RevokeSession(t.Context(), session.TokenHash)
	if err != nil {
		t.Fatal(err)
	}

	closed := make(chan string, 1)

	go func() {
		rest, _ := io.ReadAll(reader)
		closed <- string(rest)
	}()

	select {
	case rest := <-closed:
		if strings.Contains(rest, "event: done") {
			t.Errorf("a revoked stream was told the run ended, not just closed:\n%q", rest)
		}
	case <-time.After(streamHangBound):
		t.Fatal("the stream outlived the session that opened it")
	}
}
