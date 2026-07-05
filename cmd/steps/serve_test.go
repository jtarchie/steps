package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/engine"
	"github.com/jtarchie/steps/journal"
	"github.com/jtarchie/steps/machine"
	"github.com/jtarchie/steps/provider"
	"github.com/jtarchie/steps/toolreg"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// parkedRun starts summarize-critic against a never-approving mock so it
// parks at the escalate gate, and returns a server plus the parked run id.
func parkedRun(t *testing.T) (*server, string, *engine.Engine, *machine.Machine) {
	t.Helper()
	t.Chdir(t.TempDir())

	root := repoRoot()
	store, err := journal.OpenSQLite(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	eng := engine.New(store, provider.NewRegistry(), toolreg.New(), engine.NopListener{})
	mockPath := filepath.Join(t.TempDir(), "never.yaml")
	if err := os.WriteFile(mockPath, []byte(neverApprovesScript), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := provider.LoadScript(mockPath)
	if err != nil {
		t.Fatal(err)
	}
	eng.Mock = loaded

	m, err := machine.Load(filepath.Join(root, "examples/summarize-critic/workflow.ts"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Start(context.Background(), m, map[string]any{"article": "a short article about containers"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != journal.StatusParked {
		t.Fatalf("status = %s, want parked", res.Status)
	}
	return &server{store: store, eng: eng}, res.RunID, eng, m
}

const neverApprovesScript = `
draft:
  - text: '{"summary": "Draft one.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft two.", "key_points": ["a", "b", "c"]}'
  - text: '{"summary": "Draft three.", "key_points": ["a", "b", "c"]}'
critique:
  - text: '{"score": 3, "issues": ["too short"], "event": "revise"}'
  - text: '{"score": 4, "issues": ["still short"], "event": "revise"}'
  - text: '{"score": 5, "issues": ["nope"], "event": "revise"}'
`

func TestServeRunsList(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(s.store, s.eng)

	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{id, "summarize-critic", "parked", "Needs attention"} {
		if !strings.Contains(body, want) {
			t.Errorf("/runs body missing %q", want)
		}
	}
}

func TestServeRunDetailShowsGateForm(t *testing.T) {
	s, id, _, _ := parkedRun(t)
	e := newServer(s.store, s.eng)

	req := httptest.NewRequest(http.MethodGet, "/runs/"+id, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /runs/%s = %d, want 200", id, rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`name="event" value="approved"`,
		`name="event" value="rejected"`,
		"Ship the current draft as-is",
		`name="note"`,
		"Waiting on you",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("run detail missing %q\n", want)
		}
	}
}

func TestServeResumeAdvancesRun(t *testing.T) {
	s, id, eng, _ := parkedRun(t)
	e := newServer(s.store, s.eng)

	form := url.Values{"event": {"approved"}, "note": {"ship it"}}
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST resume = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/runs/"+id {
		t.Errorf("redirect = %q, want /runs/%s", loc, id)
	}

	// Resume runs in a background goroutine — poll the run row to done.
	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := eng.Store.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status == journal.StatusDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run status = %s, want done within 5s", run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServeResumeRejectsNotParked(t *testing.T) {
	s, id, eng, m := parkedRun(t)
	// Consume the park directly so the run is no longer parked.
	if _, err := eng.Resume(context.Background(), m, id, "rejected", nil); err != nil {
		t.Fatal(err)
	}
	e := newServer(s.store, s.eng)

	form := url.Values{"event": {"approved"}}
	req := httptest.NewRequest(http.MethodPost, "/runs/"+id+"/resume", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("POST resume on finished run = %d, want 409", rec.Code)
	}
}
