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

	ensureRun(ctx, t, store, placement.RunID, placement.JobName)

	err := store.RecordNode(ctx, NodeRecord{
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

// TestPlacementKeyIsScopedToItsPipeline holds the same rule one level down: it
// is the KEY that has to carry the pipeline, not only the read.
//
// Defense in depth since StartRun stopped upserting — a run id now names a row
// in exactly one pipeline — kept because the repo rule says a key carries the
// pipeline and because that should not rest on one statement in one function.
//
// merkle.HashNode folds kind, content and parent but NOT the pipeline, so two
// pipelines each running a job named build over a byte-identical task produce
// the same node hash — which is exactly why nodes is keyed (pipeline_id, hash).
// Keyed on (run_id, node_hash) alone, the second pipeline's upsert
// lands on the first's row and rewrites every column except the one saying
// whose it is: one pipeline then reports the other's machine, and the other
// reports nothing at all.
func TestPlacementKeyIsScopedToItsPipeline(t *testing.T) {
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

	// Every part of the key the two can agree on by accident: one job name, one
	// content hash, one run id.
	const (
		runID   = "SHARED01"
		jobName = "build"
	)

	placeAt(ctx, t, web, Placement{
		RunID: runID, StepName: "unit", JobName: jobName, NodeHash: hashOf(7),
		Tag: "web", Address: "ssh://web", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/web", FSType: "ext4",
	})
	placeAt(ctx, t, infra, Placement{
		RunID: runID, StepName: "unit", JobName: jobName, NodeHash: hashOf(7),
		Tag: "infra", Address: "ssh://infra", GOOS: "linux", GOARCH: "amd64",
		Workdir: "/tmp/infra", FSType: "ext4",
	})

	if got := onlyPlacement(ctx, t, web, runID).Address; got != "ssh://web" {
		t.Errorf("web's run ran on %q, want ssh://web — the other pipeline's upsert landed on its row", got)
	}

	if got := onlyPlacement(ctx, t, infra, runID).Address; got != "ssh://infra" {
		t.Errorf("infra's run ran on %q, want ssh://infra", got)
	}
}

// TestPlacementRePlacementKeepsTheMachineThatFinished covers the one path that
// writes this key twice: a step evicted off machine A and re-placed onto B
// (pipeline/venue.go's withVenueRetry). The row describes a machine, and the
// machine the work ran on is the one that finished it — so the second write
// must win outright rather than be dropped as a duplicate.
func TestPlacementRePlacementKeepsTheMachineThatFinished(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	reclaimed := "i-000000000000000aa"
	replaced := "i-000000000000000bb"

	placeAt(ctx, t, store, Placement{
		RunID: "EVICTED1", StepIndex: 0, StepName: "unit", JobName: "build", NodeHash: hashOf(1),
		Tag: "spot", Address: "aws://" + reclaimed, InstanceID: &reclaimed,
		GOOS: "linux", GOARCH: "arm64", Workdir: "/var/tmp/a", FSType: "btrfs",
		FSFree: 1 << 30, Image: "golang:1.25", BytesSent: 1_000,
	})

	err := store.RecordPlacement(ctx, Placement{
		RunID: "EVICTED1", StepIndex: 0, StepName: "unit", JobName: "build", NodeHash: hashOf(1),
		Tag: "spot", Address: "aws://" + replaced, InstanceID: &replaced,
		GOOS: "linux", GOARCH: "arm64", Workdir: "/var/tmp/b", FSType: "ext4",
		FSFree: 2 << 30, Image: "golang:1.25", BytesSent: 2_000,
	})
	if err != nil {
		t.Fatalf("RecordPlacement of the re-placement: %v", err)
	}

	got := onlyPlacement(ctx, t, store, "EVICTED1")

	if got.Address != "aws://"+replaced {
		t.Errorf("address = %q, want the machine that finished the step (%q)", got.Address, "aws://"+replaced)
	}

	if got.InstanceID == nil || *got.InstanceID != replaced {
		t.Errorf("instance_id = %q, want %q — the reclaimed machine is not where this ran",
			derefOr(got.InstanceID, "<nothing>"), replaced)
	}

	// The whole description has to move together, or the row reports a machine
	// that never existed: B's address with A's filesystem.
	if got.Workdir != "/var/tmp/b" || got.FSType != "ext4" || got.FSFree != 2<<30 || got.BytesSent != 2_000 {
		t.Errorf("workdir/fstype/fs_free/bytes_sent = %q/%q/%d/%d, want /var/tmp/b/ext4/%d/2000",
			got.Workdir, got.FSType, got.FSFree, got.BytesSent, 2<<30)
	}
}

// TestPlacementsReadBackInPlanOrder: a run's placements are a report someone
// reads top to bottom against the plan they wrote, so step 0 comes first
// whatever order the steps finished in — and a fan-out finishes them in none.
func TestPlacementsReadBackInPlanOrder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := mustOpenStore(t, filepath.Join(t.TempDir(), "state.db"))

	defer func() { _ = store.Close() }()

	// Recorded last-step-first, under hashes whose own order matches the order
	// they were written — so nothing about the storage happens to be plan order.
	for _, step := range []struct {
		index int
		hash  string
	}{
		{2, hashOf(1)},
		{0, hashOf(2)},
		{1, hashOf(3)},
	} {
		placeAt(ctx, t, store, Placement{
			RunID: "ORDER001", StepIndex: step.index, StepName: fmt.Sprintf("step-%d", step.index),
			JobName: "build", NodeHash: step.hash, Tag: "box", Address: "ssh://box",
			GOOS: "linux", GOARCH: "amd64", Workdir: "/tmp/w", FSType: "ext4",
		})
	}

	placements, err := store.RunPlacements(ctx, "ORDER001")
	if err != nil {
		t.Fatalf("RunPlacements: %v", err)
	}

	if len(placements) != 3 {
		t.Fatalf("read back %d placements, want 3: %+v", len(placements), placements)
	}

	for want, placement := range placements {
		if placement.StepIndex != want {
			t.Errorf("placement %d is step %d, want %d — the read is not in plan order",
				want, placement.StepIndex, want)
		}
	}
}

// derefOr renders a pointer column for a failure message, where nil is a real
// answer rather than a bug — see Placement.
func derefOr(value *string, absent string) string {
	if value == nil {
		return absent
	}

	return *value
}
