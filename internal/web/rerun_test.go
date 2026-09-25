package web

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
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

// TestAFanOutRunOffersRetryOnEachBuild: a version: every run holds a build per version, and Retry re-runs ONE build (fly rerun-build), so the bar cannot offer it — each build's own row does, naming its build.
func TestAFanOutRunOffersRetryOnEachBuild(t *testing.T) {
	t.Parallel()

	server, pipeline := writableServerWithPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, "fan", "build", "/tmp/ws", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	for build, version := range []string{`{"n":"1"}`, `{"n":"2"}`} {
		err = pipeline.Store.RecordRunInput(ctx, "fan", fmt.Sprintf("fan#%d", build), "repo", "repo", version)
		if err != nil {
			t.Fatalf("RecordRunInput: %v", err)
		}
	}

	appendEvents(t, pipeline.Store, "fan", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "repo", StepKind: "get", StepID: 2, Status: "failed"},
	})

	err = pipeline.Store.FinishRun(ctx, "fan", "failed")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, page := get(t, server, "/p/demo/runs/fan")

	if strings.Contains(actionBar(t, page), "Retry") {
		t.Error("the bar offers Retry for a run of several builds, which one would it re-run?")
	}

	for build := range 2 {
		want := fmt.Sprintf(`<input type="hidden" name="build" value="%d"><button class="btn" type="submit" title="run build #%d again, with the same inputs">⟲ Retry</button>`, build, build)
		if !strings.Contains(page, want) {
			t.Errorf("build #%d's row offers no Retry of itself", build)
		}
	}
}
