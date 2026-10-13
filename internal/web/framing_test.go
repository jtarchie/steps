package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestEveryResponseRefusesFraming: the origin check cannot see a click inside a framed copy of the UI — that POST is same-origin — so a one-click trigger, approve or abort form is clickjackable unless the browser refuses to frame the page at all.
func TestEveryResponseRefusesFraming(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "run-framed", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-framed", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "first", StepKind: "task", StepID: 1, Status: "succeeded"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-framed", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	want := map[string]string{
		"Content-Security-Policy": "frame-ancestors 'none'",
		"X-Frame-Options":         "DENY",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	}

	for _, tc := range []struct {
		name, method, path, origin string
		code                       int
	}{
		{"page", http.MethodGet, "/p/demo", "", http.StatusOK},
		{"asset", http.MethodGet, "/static/htmx.min.js", "", http.StatusOK},
		{"sse", http.MethodGet, "/p/demo/runs/run-framed/events?after=0", "", http.StatusOK},
		{"api", http.MethodGet, "/api/pipelines", "", http.StatusOK},
		{"refused", http.MethodPost, "/p/demo/jobs/build/trigger", "https://evil.example", http.StatusForbidden},
		{"unrouted", http.MethodGet, "/nowhere", "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			done := make(chan *httptest.ResponseRecorder, 1)

			go func() {
				req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
				if tc.origin != "" {
					req.Header.Set("Origin", tc.origin)
				}

				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, req)
				done <- rec
			}()

			var rec *httptest.ResponseRecorder

			select {
			case rec = <-done:
			case <-time.After(streamHangBound):
				t.Fatalf("%s did not finish", tc.path)
			}

			if rec.Code != tc.code {
				t.Fatalf("%s: status %d, want %d", tc.path, rec.Code, tc.code)
			}

			for header, value := range want {
				if got := rec.Header().Get(header); got != value {
					t.Errorf("%s: %s = %q, want %q", tc.path, header, got, value)
				}
			}
		})
	}
}
