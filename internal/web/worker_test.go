package web

// Drawing where a step ran.

import (
	"context"
	"encoding/json"
	"net/http"
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

// TestRunPageDrawsTheMachines is the browser half of what `steps runs
// --where` answers in a terminal: the two front ends read the same rows, and
// a record only one of them can see is half a record.
func TestRunPageDrawsTheMachines(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "placed", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	hash := strings.Repeat("a", 64)

	err = pipeline.Store.RecordNode(ctx, store.NodeRecord{
		Hash: hash, Kind: "task", StepIndex: 0, Resource: "compile",
		Content: map[string]any{"body": "x"},
	}, "build", "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	instance := "i-0abc123def4567890"
	root := 0

	err = pipeline.Store.RecordPlacement(ctx, store.Placement{
		RunID: "placed", StepIndex: 0, StepName: "compile", JobName: "build",
		NodeHash: hash, Tag: "gpu", Address: "aws://" + instance, InstanceID: &instance,
		GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/steps/work", FSType: "btrfs", FSFree: 41_083_355_136,
		UID: &root, GID: &root, Image: "golang:1.25", BytesSent: 67_108_864,
	})
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, "placed", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	code, body := get(t, server, "/p/demo/runs/placed")
	if code != http.StatusOK {
		t.Fatalf("GET run = %d: %s", code, body)
	}

	for _, want := range []string{
		"gpu", "linux/arm64", "btrfs (38.3 GiB free)", "64.0 MiB",
		"0:0", "aws://" + instance + " in golang:1.25",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the run page does not show %q", want)
		}
	}
}

// TestRunPageKeepsTheMachinesPanelOffAnUnplacedRun: a pipeline that names no
// worker is the ordinary case, and an empty panel on every one of those pages
// reads as a broken record rather than as nothing to report.
func TestRunPageKeepsTheMachinesPanelOffAnUnplacedRun(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, "local", "build", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = pipeline.Store.FinishRun(ctx, "local", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	_, body := get(t, server, "/p/demo/runs/local")

	if strings.Contains(body, "placed step(s)") {
		t.Error("an unplaced run draws the machines panel")
	}
}

// TestPlacementStatesWhatTheWorkerCouldNotSay: an empty fstype is a shim that
// has no answer — an older one, or a platform with no statfs — and never an
// ordinary disk. A blank cell hides exactly the case the column exists for.
func TestPlacementStatesWhatTheWorkerCouldNotSay(t *testing.T) {
	t.Parallel()

	silent := placementView{}
	if got := silent.Filesystem(); got != "not reported" {
		t.Errorf("Filesystem with no answer = %q, want a stated silence", got)
	}

	if got := silent.Identity(); got != "" {
		t.Errorf("Identity with no answer = %q, want nothing — an invented 0:0 reads as root", got)
	}

	// tmpfs is marked, because it is memory: the pushed binary and the step's
	// tree spend the machine's RAM and a reboot loses both.
	if !(placementView{Placement: store.Placement{FSType: "tmpfs"}}).Volatile() {
		t.Error("a tmpfs workdir is not marked volatile")
	}

	if (placementView{Placement: store.Placement{FSType: "btrfs"}}).Volatile() {
		t.Error("an ordinary disk is marked volatile")
	}
}

// TestPlacementRendersTheBareCases: an ssh:// worker running a step on the
// host names one thing, not two, and a step whose inputs the worker ALREADY
// HELD honestly cost nothing to reach — the panel has to be able to say 0 B
// without it reading as a broken counter.
func TestPlacementRendersTheBareCases(t *testing.T) {
	t.Parallel()

	bare := placementView{Placement: store.Placement{Address: "ssh://box"}}
	if got := bare.Machine(); got != "ssh://box" {
		t.Errorf("Machine = %q, want just the host — there is no container to name", got)
	}

	for _, want := range []struct {
		bytes int64
		text  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{41_083_355_136, "38.3 GiB"},
	} {
		if got := formatBinaryBytes(want.bytes); got != want.text {
			t.Errorf("formatBinaryBytes(%d) = %q, want %q", want.bytes, got, want.text)
		}
	}
}
