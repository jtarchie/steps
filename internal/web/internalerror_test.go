package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

const leakyDetail = "open /var/lib/steps/state.db: disk I/O error"

type brokenRunReads struct{ store.Store }

func (brokenRunReads) FindRunRow(context.Context, string) (store.RunRow, bool, error) {
	return store.RunRow{}, false, errors.New(leakyDetail)
}

// A 500's text is whatever the failing layer said, so SQL and paths reach whoever triggered it; the log keeps the detail and the viewer gets the fact.
func TestServerErrorsDoNotLeakTheirDetail(t *testing.T) {
	t.Parallel()

	server, manager := managedServer(t)
	manager.setErr = errors.New(leakyDetail)

	code, body := call(t, server, http.MethodPut, "/api/pipelines/app", `{"source":"jobs: []"}`)
	if code != http.StatusInternalServerError || strings.Contains(body, "/var/lib") {
		t.Errorf("the API answered %d %q for a failed set, want a 500 without the detail", code, body)
	}

	_, pipeline := testPipeline(t)
	pipeline.Store = brokenRunReads{pipeline.Store}

	page, err := New([]*Pipeline{pipeline}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	status, text := get(t, page, "/p/demo/runs/anything/events")
	if status != http.StatusInternalServerError || strings.Contains(text, "/var/lib") {
		t.Errorf("a page answered %d for a failed read, want a 500 without the detail:\n%s", status, text)
	}
}
