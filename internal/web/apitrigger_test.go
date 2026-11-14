package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/scheduler"
)

// pinRecorder is enqueueRecorder remembering what each pinned trigger named.
type pinRecorder struct {
	enqueueRecorder

	pins []string
}

func (r *pinRecorder) EnqueuePinned(_ context.Context, _ *scheduler.Pipeline, job, reason string, pin map[string]string) error {
	r.pins = append(r.pins, fmt.Sprintf("%s %s %v", job, reason, pin))

	return nil
}

func postJSON(t *testing.T, server *Server, target, body string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code
}

// A terminal's trigger is the button's (#235): the same refusals, the pin handed on whole, and a body that is not one never read as "no pin".
func TestAPITriggerQueuesWhatWasAskedFor(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)
	runner := &pinRecorder{}

	server, err := New([]*Pipeline{pipeline}, runner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		target, body string
		want         int
	}{
		{"/api/pipelines/demo/jobs/build/trigger", ``, http.StatusAccepted},
		{"/api/pipelines/demo/jobs/build/trigger", `{"force":true}`, http.StatusAccepted},
		{"/api/pipelines/demo/jobs/build/trigger", `{"pin":{"number":"87"}}`, http.StatusAccepted},
		{"/api/pipelines/demo/jobs/build/trigger", `{"pin":{"number":"87"},"force":true}`, http.StatusBadRequest},
		{"/api/pipelines/demo/jobs/build/trigger", `{"pin":`, http.StatusBadRequest},
		{"/api/pipelines/demo/jobs/nope/trigger", ``, http.StatusNotFound},
		{"/api/pipelines/nobody/jobs/build/trigger", ``, http.StatusNotFound},
		{"/api/pipelines/demo/runs/nope/rerun", ``, http.StatusNotFound},
	} {
		if got := postJSON(t, server, tc.target, tc.body); got != tc.want {
			t.Errorf("POST %s %s = %d, want %d", tc.target, tc.body, got, tc.want)
		}
	}

	if got := strings.Join(runner.reasons, "|"); got != "trigger (cli)|trigger, no cache (cli)" {
		t.Errorf("unpinned reasons = %q", got)
	}

	if got := strings.Join(runner.pins, "|"); got != "build trigger, pinned (cli) map[number:87]" {
		t.Errorf("pinned = %q", got)
	}
}

// --read-only withholds starting work from a terminal as it does from the page.
func TestAPITriggerIsWithheldFromAReadOnlyServer(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, target := range []string{"/api/pipelines/demo/jobs/build/trigger", "/api/pipelines/demo/runs/some-run/rerun"} {
		if status := postJSON(t, server, target, ""); status != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", target, status)
		}
	}
}
