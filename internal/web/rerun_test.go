package web

import (
	"net/http"
	"testing"
)

// TestRetryIsRefusedWhereATriggerIs: a paused pipeline starts nothing new, however it is asked (Concourse would hold it pending; steps refuses, as it refuses a trigger), a running run is aborted rather than retried, and a read-only server changes nothing.
func TestRetryIsRefusedWhereATriggerIs(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)
	readOnly, _ := testPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), "live", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	if code := post(t, server, "/p/demo/runs/live/rerun", nil); code != http.StatusConflict {
		t.Errorf("retry of a running run = %d, want 409", code)
	}

	err = pipeline.Store.FinishRun(t.Context(), "live", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	if code := post(t, server, "/p/demo/runs/live/rerun", nil); code != http.StatusSeeOther {
		t.Errorf("retry of a finished run = %d, want 303 to the follow page", code)
	}

	err = pipeline.Store.Pause(t.Context())
	if err != nil {
		t.Fatalf("Pause: %v", err)
	}

	if code := post(t, server, "/p/demo/runs/live/rerun", nil); code != http.StatusConflict {
		t.Errorf("retry in a paused pipeline = %d, want 409", code)
	}

	if code := post(t, readOnly, "/p/demo/runs/live/rerun", nil); code != http.StatusForbidden {
		t.Errorf("retry on a read-only server = %d, want 403", code)
	}

	if code := post(t, server, "/p/demo/runs/nosuch/rerun", nil); code != http.StatusNotFound {
		t.Errorf("retry of an unknown run = %d, want 404", code)
	}
}
