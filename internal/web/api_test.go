package web

// The endpoints `steps pipeline` talks to, and the registry they mutate.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// fakeManager lets a route be tested with no store driver and no workspace behind it.
type fakeManager struct {
	set      []string
	destroy  []string
	renamed  [][2]string
	setErr   error
	otherErr error
}

func (f *fakeManager) Set(_ context.Context, name string, _ SetRequest) (SetResult, error) {
	f.set = append(f.set, name)

	if f.setErr != nil {
		return SetResult{}, f.setErr
	}

	return SetResult{SHA: "sha-one", Created: true}, nil
}

func (f *fakeManager) Destroy(_ context.Context, name string) error {
	f.destroy = append(f.destroy, name)

	return f.otherErr
}

func (f *fakeManager) Rename(_ context.Context, from, to string) error {
	f.renamed = append(f.renamed, [2]string{from, to})

	return f.otherErr
}

// managedServer is a daemon as it starts: a manager attached, nothing served.
func managedServer(t *testing.T) (*Server, *fakeManager) {
	t.Helper()

	server, err := New(nil, stubRunner{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	manager := &fakeManager{}
	server.SetManager(manager)

	return server, manager
}

func call(t *testing.T, server *Server, method, target, body string) (int, string) {
	t.Helper()

	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	var req *http.Request

	if reader != nil {
		req = httptest.NewRequestWithContext(t.Context(), method, target, reader)
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequestWithContext(t.Context(), method, target, nil)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// TestAnEmptyDaemonSaysHowToFillIt: with nothing set there is nothing to redirect to, and the page saying what to type is the whole of an empty daemon's UI.
func TestAnEmptyDaemonSaysHowToFillIt(t *testing.T) {
	t.Parallel()

	server, _ := managedServer(t)

	code, body := call(t, server, http.MethodGet, "/", "")
	if code != http.StatusOK {
		t.Fatalf("the root of an empty daemon = %d, want 200", code)
	}

	if !strings.Contains(body, "steps pipeline set") {
		t.Errorf("an empty daemon does not say how to set a pipeline:\n%s", body)
	}
}

// TestSetRefusesANameThatBreaksARoute: the name is concatenated into /p/<name> and stored as an identity, so what breaks either is refused before anything is opened.
func TestSetRefusesANameThatBreaksARoute(t *testing.T) {
	t.Parallel()

	server, manager := managedServer(t)

	code, _ := call(t, server, http.MethodPut, "/api/pipelines/..", `{"source":"jobs: []"}`)
	if code != http.StatusBadRequest {
		t.Errorf("a traversal name answered %d, want 400", code)
	}

	if len(manager.set) != 0 {
		t.Errorf("the manager was asked to set %v", manager.set)
	}
}

// TestSetRefusalsCarryTheirMeaning: a sender acts differently on "it moved under you" and "it is wrong", so the two cannot share a status.
func TestSetRefusalsCarryTheirMeaning(t *testing.T) {
	t.Parallel()

	for _, probe := range []struct {
		err  error
		want int
	}{
		{ErrRevisionMoved, http.StatusConflict},
		{ErrRefused, http.StatusUnprocessableEntity},
	} {
		server, manager := managedServer(t)
		manager.setErr = probe.err

		code, body := call(t, server, http.MethodPut, "/api/pipelines/app", `{"source":"jobs: []"}`)
		if code != probe.want {
			t.Errorf("%v answered %d, want %d", probe.err, code, probe.want)
		}

		// JSON rather than a page: this answers a terminal, and HTML is a set that cannot say why it was refused.
		var wrapped struct {
			Message string `json:"message"`
		}

		if json.Unmarshal([]byte(body), &wrapped) != nil || wrapped.Message == "" {
			t.Errorf("the API answered a refusal as %q, want a JSON message", body)
		}
	}
}

// TestPipelineVerbsRefuseAPipelineTheDaemonDoesNotHold: these are all about one that exists, and any other answer makes a typo look like a working command.
func TestPipelineVerbsRefuseAPipelineTheDaemonDoesNotHold(t *testing.T) {
	t.Parallel()

	server, _ := managedServer(t)

	for _, probe := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/pipelines/absent", ""},
		{http.MethodPost, "/api/pipelines/absent/pause", ""},
		{http.MethodPost, "/api/pipelines/absent/unpause", ""},
	} {
		code, _ := call(t, server, probe.method, probe.target, probe.body)
		if code != http.StatusNotFound {
			t.Errorf("%s %s answered %d, want 404", probe.method, probe.target, code)
		}
	}
}

// TestTheRegistryIsWhatARouteResolves: serving a pipeline the moment it is set, and not a moment after it is destroyed, is the whole difference from the fixed list this used to be.
func TestTheRegistryIsWhatARouteResolves(t *testing.T) {
	t.Parallel()

	server, _ := managedServer(t)

	if code, _ := call(t, server, http.MethodGet, "/p/demo", ""); code != http.StatusNotFound {
		t.Fatalf("/p/demo answered %d before anything was added", code)
	}

	_, target := testPipeline(t)

	err := server.Add(target)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	if code, _ := call(t, server, http.MethodGet, "/p/demo", ""); code != http.StatusOK {
		t.Errorf("/p/demo answered %d after it was added", code)
	}

	// Two pipelines under one name would share a route, a store scope and an agent pin scope, so the second is refused rather than shadowing.
	err = server.Add(NewPipeline("demo", "other.yml", &config.Config{}, nil, nil))
	if err == nil {
		t.Error("a second pipeline claimed a name this daemon already serves")
	}

	if server.Remove("demo") == nil {
		t.Fatal("Remove did not hand back the pipeline it was serving")
	}

	if code, _ := call(t, server, http.MethodGet, "/p/demo", ""); code != http.StatusNotFound {
		t.Errorf("/p/demo still answers after it was removed")
	}
}

// TestAServerWithNoManagerRefusesTheVerbs: a server built with nothing to apply a set with says so, rather than panicking on a nil call.
func TestAServerWithNoManagerRefusesTheVerbs(t *testing.T) {
	t.Parallel()

	server, err := New(nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, probe := range []struct{ method, target string }{
		{http.MethodPut, "/api/pipelines/app"},
		{http.MethodDelete, "/api/pipelines/app"},
		{http.MethodPost, "/api/pipelines/app/pause"},
	} {
		code, _ := call(t, server, probe.method, probe.target, `{"source":"jobs: []"}`)
		if code != http.StatusForbidden {
			t.Errorf("%s %s answered %d with no manager, want 403", probe.method, probe.target, code)
		}
	}
}
