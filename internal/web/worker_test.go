package web

// Drawing where a step ran.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestRunViewCarriesTheWorker pins that the post-hoc view says where a placed
// step ran, and says nothing for one that ran here.
func TestRunViewCarriesTheWorker(t *testing.T) {
	t.Parallel()

	rows := []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "here", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "here", StepID: 1, Status: "succeeded"},
		{Type: events.TypeStepStarted, StepIndex: 1, StepName: "there", StepID: 2},
		{Type: events.TypeStepFinished, StepIndex: 1, StepName: "there", StepID: 2, Status: "failed",
			Worker: "gpu (ssh://jt@box)"},
	}

	view := buildRunView(store.RunRow{ID: "R1"}, rows, nil)

	byName := map[string]*stepView{}
	for _, step := range view.Steps {
		byName[step.Name] = step
	}

	if got := byName["there"].Worker; got != "gpu (ssh://jt@box)" {
		t.Errorf("placed step worker = %q, want the machine it ran on", got)
	}

	if got := byName["here"].Worker; got != "" {
		t.Errorf("local step worker = %q, want nothing — naming every local step would bury the ones that left", got)
	}
}

// TestLiveStreamDrawsTheWorkerToo is this package's standing rule: anything
// the server draws for a finished step, the stream has to draw too, or a
// reader watching live and one who reloaded see different rows.
func TestLiveStreamDrawsTheWorkerToo(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(liveEvent{RunEventRow: store.RunEventRow{
		Type: events.TypeStepFinished, StepName: "there", Status: "succeeded",
		Worker: "gpu (ssh://jt@box)",
	}})
	if err != nil {
		t.Fatalf("marshalling a live event: %v", err)
	}

	var payload map[string]any

	err = json.Unmarshal(encoded, &payload)
	if err != nil {
		t.Fatalf("decoding the live payload: %v", err)
	}

	if got, _ := payload["worker"].(string); got != "gpu (ssh://jt@box)" {
		t.Fatalf("the live payload carried worker %v, want the machine", payload["worker"])
	}

	// And the client has to do something with it.
	source, err := assets.ReadFile("templates/run.html")
	if err != nil {
		t.Fatalf("reading the run template: %v", err)
	}

	if !strings.Contains(string(source), "e.worker") {
		t.Error("the live renderer never reads the field, so a step that finishes while watching names no machine until reload")
	}
}
