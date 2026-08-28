package store

// What a placement row must be able to say, and what it must refuse to invent.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// placeAt records a run, a node and a placement against store, returning the
// run id.
func placeAt(ctx context.Context, t *testing.T, store *Store, placement Placement) {
	t.Helper()

	err := store.StartRun(ctx, placement.RunID, placement.JobName, "/tmp/ws")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	err = store.RecordNode(ctx, NodeRecord{
		Hash: placement.NodeHash, Kind: "task", StepIndex: placement.StepIndex,
		Resource: placement.StepName, Content: map[string]any{"body": "x"},
	}, placement.JobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	err = store.RecordPlacement(ctx, placement)
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}
}

// TestPlacementDistinguishesAbsentFromZero is the reason three columns are
// pointers.
//
// uid 0 is root, which is the ordinary answer under the aws:// bootstrap, and
// a shim on a platform that cannot say reports nothing at all. Stored as
// plain integers those two collapse into the same row, and they demand
// opposite readings: "this ran as root" is a finding, "we do not know who
// this ran as" is a gap.
func TestPlacementDistinguishesAbsentFromZero(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	instance := "i-0123456789abcdef0"
	root := 0

	placeAt(ctx, t, store, Placement{
		RunID: "AAAA1111", StepIndex: 0, StepName: "on-ec2", JobName: "build",
		NodeHash: hashOf(1), Tag: "aws", Address: "aws://" + instance,
		InstanceID: &instance, GOOS: "linux", GOARCH: "arm64",
		Workdir: "/var/tmp/steps/work", FSType: "btrfs", FSFree: 1 << 35,
		UID: &root, GID: &root, Image: "golang:1.25", BytesSent: 4096,
	})

	// A machine steps did not acquire, run by a user the shim did not name.
	placeAt(ctx, t, store, Placement{
		RunID: "BBBB2222", StepIndex: 0, StepName: "on-ssh", JobName: "build",
		NodeHash: hashOf(2), Tag: "box", Address: "ssh://box",
		GOOS: "linux", GOARCH: "amd64", Workdir: "/tmp/steps/work",
		FSType: "ext4", FSFree: 1 << 30, BytesSent: 512,
	})

	onEC2 := onlyPlacement(ctx, t, store, "AAAA1111")

	if onEC2.InstanceID == nil || *onEC2.InstanceID != instance {
		t.Errorf("instance_id = %v, want %q", onEC2.InstanceID, instance)
	}

	if onEC2.UID == nil || *onEC2.UID != 0 {
		t.Errorf("uid = %v, want a reported 0 — root is not the same as silence", onEC2.UID)
	}

	onSSH := onlyPlacement(ctx, t, store, "BBBB2222")

	if onSSH.InstanceID != nil {
		t.Errorf("instance_id = %q for an ssh:// worker, want nothing — there is no instance to name", *onSSH.InstanceID)
	}

	if onSSH.UID != nil {
		t.Errorf("uid = %d for a shim that did not say, want nothing — an invented 0 reads as root", *onSSH.UID)
	}
}

// TestPlacementsAreScopedToTheirPipeline holds the repo's rule that a query in
// this package without a pipeline_id predicate is a bug. One state file may
// hold several pipelines, and run ids are minted per pipeline, so an unscoped
// read hands one pipeline's report another's machines.
func TestPlacementsAreScopedToTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	web, err := OpenStore(path, "web")
	if err != nil {
		t.Fatalf("OpenStore web: %v", err)
	}

	defer func() { _ = web.Close() }()

	infra, err := OpenStore(path, "infra")
	if err != nil {
		t.Fatalf("OpenStore infra: %v", err)
	}

	defer func() { _ = infra.Close() }()

	// The same run id in both pipelines, which is what makes this a real
	// collision rather than a coincidence: ids are minted per pipeline.
	placeAt(ctx, t, web, Placement{
		RunID: "SHARED01", StepName: "web-step", JobName: "build", NodeHash: hashOf(1),
		Tag: "web", Address: "ssh://web", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/w", FSType: "ext4",
	})
	placeAt(ctx, t, infra, Placement{
		RunID: "SHARED01", StepName: "infra-step", JobName: "build", NodeHash: hashOf(2),
		Tag: "infra", Address: "ssh://infra", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/i", FSType: "ext4",
	})

	if got := onlyPlacement(ctx, t, web, "SHARED01").StepName; got != "web-step" {
		t.Errorf("web's run reported step %q — it is reading another pipeline's placements", got)
	}

	if got := onlyPlacement(ctx, t, infra, "SHARED01").StepName; got != "infra-step" {
		t.Errorf("infra's run reported step %q — it is reading another pipeline's placements", got)
	}
}

func onlyPlacement(ctx context.Context, t *testing.T, store *Store, runID string) Placement {
	t.Helper()

	placements, err := store.RunPlacements(ctx, runID)
	if err != nil {
		t.Fatalf("RunPlacements: %v", err)
	}

	if len(placements) != 1 {
		t.Fatalf("run %s reported %d placements, want 1: %+v", runID, len(placements), placements)
	}

	return placements[0]
}

func hashOf(n int) string { return fmt.Sprintf("%064x", n) }
