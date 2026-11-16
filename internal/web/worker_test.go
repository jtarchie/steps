package web

// Drawing where a step ran.

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestLiveStreamDrawsTheWorkerToo is this package's standing rule: anything
// the server draws for a finished step, the stream has to draw too, or a
// reader watching live and one who reloaded see different rows. It holds now
// because the stream renders the row with the page's own template — this test
// is what says so out loud.
func TestLiveStreamDrawsTheWorkerToo(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-worker", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-worker", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "there", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "there", StepKind: "task", StepID: 1,
			Status: "succeeded", Worker: "gpu (ssh://jt@box)"},
	})

	err = pipeline.Store.FinishRun(ctx, "run-worker", "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	stream := sseHTML(streamOf(t, server, "/p/demo/runs/run-worker/events"))
	if !strings.Contains(stream, `on gpu (ssh://jt@box)`) {
		t.Errorf("a step that finishes while watching names no machine until reload:\n%s", stream)
	}
}

// TestRunPageDrawsTheMachines is the browser half of what `steps runs
// --where` answers in a terminal: the two front ends read the same rows, and
// a record only one of them can see is half a record.
func TestRunPageDrawsTheMachines(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "placed", JobName: "build"})
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

	appendEvents(t, pipeline.Store, "placed", []store.RunEventRow{
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", StepID: 1, Status: "succeeded", Hash: hash, Worker: "gpu (aws://i-0abc123def4567890)"},
	})

	instance := "i-0abc123def4567890"
	root := 0

	err = pipeline.Store.RecordPlacement(ctx, store.Placement{
		RunID: "placed", StepIndex: 0, StepName: "compile", JobName: "build",
		NodeHash: hash, Slot: hash, Tag: "gpu", Address: "aws://" + instance, InstanceID: &instance,
		GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/steps/work",
		UID:     &root, GID: &root, Image: "golang:1.25", ImageDigest: "sha256:0123456789abcdef0123", BytesSent: 67_108_864, BytesReceived: 1_048_576,
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

	// The identity is asserted WITH its cell, because a bare "0:0" is also a
	// substring of the run's own timestamp — 3210 of a day's 86400 stamps
	// contain one — so the loose form passed with the cell deleted, and the
	// uid-0-is-root-not-silence contract went unproven on those days.
	for _, want := range []string{
		"gpu", "linux/arm64", "64.0 MiB", "1.0 MiB",
		">0:0<", "1.0 MiB back",
		// #239: on the row itself, so which image ran is read without opening the step; the digest rides its tooltip and the placed-on line, not the row.
		`<span class="note worker" title="golang:1.25@sha256:0123456789ab"><span class="on">on gpu (aws://i-0abc123def4567890)</span> · <span class="img">golang:1.25</span></span>`,
		// The image leads the detail line, and the row already names the worker, so the machine follows without saying gpu twice.
		`<div class="agentmeta">image golang:1.25@sha256:0123456789ab · aws://` + instance + ` · linux/arm64`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the run page does not show %q", want)
		}
	}
}

// TestImageRefShortensTheDigestAndNeverRepeatsIt: the row shows enough of the digest to tell two pulls of one tag apart, and an image already written by digest is left as written.
func TestImageRefShortensTheDigestAndNeverRepeatsIt(t *testing.T) {
	t.Parallel()

	const digest = "sha256:0123456789abcdef0123456789abcdef"

	for name, tc := range map[string]struct{ image, digest, want string }{
		"a tag":              {"golang:1.27", digest, "golang:1.27@sha256:0123456789ab"},
		"pinned by digest":   {"golang@" + digest, digest, "golang@" + digest},
		"no digest known":    {"golang:1.27", "", "golang:1.27"},
		"no container":       {"", digest, ""},
		"a digest too short": {"golang:1.27", "sha256:abc", "golang:1.27"},
	} {
		got := PlacementView{Placement: store.Placement{Image: tc.image, ImageDigest: tc.digest}}.ImageRef()
		if got != tc.want {
			t.Errorf("%s: ImageRef = %q, want %q", name, got, tc.want)
		}
	}
}

// TestShortImageCutsOnlyADigestPin: a row names a tag as written, since it is short and says more than its digest, and cuts an image pinned by digest to the twelve characters Docker shows for an id.
func TestShortImageCutsOnlyADigestPin(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ image, want string }{
		"a tag":                {"golang:1.27", "golang:1.27"},
		"pinned by digest":     {"localhost:5050/backerkit@sha256:af8016cedcbfed4c8c85", "[af8016cedcbf]"},
		"a pin too short":      {"golang@sha256:abc", "golang@sha256:abc"},
		"a pin with no scheme": {"golang@af8016cedcbfed4c", "golang@af8016cedcbfed4c"},
		"no container":         {"", ""},
	} {
		got := PlacementView{Placement: store.Placement{Image: tc.image, ImageDigest: "sha256:0123456789abcdef0123"}}.ShortImage()
		if got != tc.want {
			t.Errorf("%s: ShortImage = %q, want %q", name, got, tc.want)
		}
	}
}

// TestPlacementStatesWhatTheWorkerCouldNotSay: a worker that named no identity is shown none, never an invented one.
func TestPlacementStatesWhatTheWorkerCouldNotSay(t *testing.T) {
	t.Parallel()

	silent := PlacementView{}

	if got := silent.Identity(); got != "" {
		t.Errorf("Identity with no answer = %q, want nothing — an invented 0:0 reads as root", got)
	}

	if got := silent.Disk(); got != "-" {
		t.Errorf("Disk with nothing weighed = %q, want - — an unweighed worker is not an empty one", got)
	}

	var none int64
	if got := (PlacementView{Placement: store.Placement{VolumeBytes: &none}}).Disk(); got != "0 B" {
		t.Errorf("Disk of a measured zero = %q, want 0 B", got)
	}
}

// TestPlacementRendersTheBareCases: an ssh:// worker running a step on the
// host names one thing, not two.
func TestPlacementRendersTheBareCases(t *testing.T) {
	t.Parallel()

	bare := PlacementView{Placement: store.Placement{Address: "ssh://box"}}
	if got := bare.Machine(); got != "ssh://box" {
		t.Errorf("Machine = %q, want just the host — there is no container to name", got)
	}
}

// TestFormatBinaryBytesCoversEveryUnitAndItsBoundary is the ONE table for
// this formatter: the run page and `steps runs --where` print the same sizes
// through it, and the two copies it replaces had already disagreed.
//
// A step whose inputs the worker ALREADY HELD honestly cost nothing to reach,
// so 0 B has to render without reading as a broken counter — and the
// boundary cases are the bug the merge found.
func TestFormatBinaryBytesCoversEveryUnitAndItsBoundary(t *testing.T) {
	t.Parallel()

	for _, want := range []struct {
		bytes int64
		text  string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{67 << 20, "67.0 MiB"},
		{41_083_355_136, "38.3 GiB"},
		// One byte short of the next unit: the divide-while-over-1024 loop
		// stops here, and %.1f then rounds the remainder back over the
		// boundary — 1024.0 MiB is not a size anything else reports.
		{(1 << 20) - 1, "1.0 MiB"},
		{(1 << 30) - 1, "1.0 GiB"},
		{(1 << 40) - 1, "1.0 TiB"},
		// Clamped at TiB rather than running off the unit table.
		{5 << 50, "5120.0 TiB"},
	} {
		if got := FormatBinaryBytes(want.bytes); got != want.text {
			t.Errorf("FormatBinaryBytes(%d) = %q, want %q", want.bytes, got, want.text)
		}
	}
}

// TestAStepNamesItsImageOnceWhereverItRan: the image note becomes the start of the placement line, which leads an agent's body because its conversation would otherwise push it a screen below the row; with no placement line, the note is still said.
func TestAStepNamesItsImageOnceWhereverItRan(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := t.Context()
	hash := strings.Repeat("d", 64)
	note := store.RunEventRow{Type: events.TypeStepNote, StepIndex: 0, StepID: 1, Status: events.NoteInfo, Text: "image: toolchain → alpine@sha256:abc", Name: events.NoteImage, Detail: "toolchain"}

	for _, runID := range []string{"placed", "unplaced"} {
		err := pipeline.Store.StartRun(ctx, store.NewRun{ID: runID, JobName: "build", Workspace: "/tmp/ws"})
		if err != nil {
			t.Fatal(err)
		}

		appendEvents(t, pipeline.Store, runID, []store.RunEventRow{
			{Type: events.TypeStepStarted, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1},
			note,
			{Type: events.TypeAgentUser, StepIndex: 0, StepName: "review", StepID: 1, Text: "Review it."},
			{Type: events.TypeStepFinished, StepIndex: 0, StepName: "review", StepKind: "agent", StepID: 1, Status: "succeeded", Hash: hash},
		})
	}

	mustRecordResult(t, pipeline, hash, map[string]any{})

	err := pipeline.Store.RecordPlacement(ctx, store.Placement{
		RunID: "placed", StepIndex: 0, StepName: "review", JobName: "build",
		NodeHash: hash, Slot: hash, Address: "local:", GOOS: "linux", GOARCH: "arm64", Image: "alpine@sha256:abc",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, placed := get(t, server, "/p/demo/runs/placed")
	line := strings.Index(placed, `<div class="agentmeta">image toolchain → alpine@sha256:abc · local: · linux/arm64`)
	turn := strings.Index(placed, `<div class="turn user`)

	if line < 0 || turn < 0 || line > turn {
		t.Errorf("the agent's placement line (at %d) does not lead its conversation (at %d)", line, turn)
	}

	if strings.Contains(placed, `class="log notice">image:`) {
		t.Error("the image note is said beside the placement line that already says it")
	}

	_, unplaced := get(t, server, "/p/demo/runs/unplaced")
	if !strings.Contains(unplaced, `class="log notice">image: toolchain → alpine@sha256:abc<`) {
		t.Error("a step with no placement line lost the note naming its image")
	}
}
