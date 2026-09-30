package storetest

// Which rows retention keeps, as a caller of the contract sees them. Lifted
// from the sqlite driver's footprint tests, which keep what only sqlite can
// measure (dbstat, backdated rows); what is here needs no SQL, so every
// driver answers it.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// nodesPerRetainedRun is the node cache every driver keeps per run
// run_history: retains. It is observable — a job's node listing is capped at
// it — so it is contract, and a driver with another multiplier fails here.
const nodesPerRetainedRun = 20

// build records one finished run of a job the way a build does: the run, a
// node, the step that recorded it, and the events naming it.
func build(ctx context.Context, t *testing.T, st store.Store, jobName, runID string, n int) {
	t.Helper()

	mustStartRuns(ctx, t, st, jobName, runID)

	hash := hashOf(1_000_000 + n)

	err := st.RecordNode(ctx, store.NodeRecord{
		Hash: hash, Kind: "task", Resource: "work", Content: map[string]any{"hash": hash},
	}, jobName, "succeeded", nil, nil)
	if err != nil {
		t.Fatalf("RecordNode: %v", err)
	}

	err = st.RecordRunStep(ctx, runID, runID+"#0", 0, "work")
	if err != nil {
		t.Fatalf("RecordRunStep: %v", err)
	}

	for _, eventType := range []string{"step_started", "step_finished"} {
		err = st.AppendRunEvent(ctx, store.RunEventRow{
			RunID: runID, Type: eventType, StepName: "work", StepKind: "task",
			Status: "succeeded", Hash: hash, At: time.Now(),
		})
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}

	err = st.FinishRun(ctx, runID, "succeeded")
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
}

func runIDs(t *testing.T, st store.Store, jobName string) []string {
	t.Helper()

	runs, err := st.ListRuns(context.Background(), jobName, 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}

	ids := make([]string, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}

	slices.Sort(ids)

	return ids
}

func assertRunSurvives(t *testing.T, st store.Store, runID, what string) {
	t.Helper()

	_, found, err := st.FindRunRow(context.Background(), runID)
	if err != nil {
		t.Fatalf("FindRunRow(%q): %v", runID, err)
	}

	if !found {
		t.Fatal(what)
	}
}

func countNodes(t *testing.T, st store.Store, jobName string) int {
	t.Helper()

	nodes, err := st.ListNodes(context.Background(), jobName, 0)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}

	return len(nodes)
}

func mustPlace(t *testing.T, st store.Store, placement store.Placement) {
	t.Helper()

	err := st.RecordPlacement(context.Background(), placement)
	if err != nil {
		t.Fatalf("RecordPlacement: %v", err)
	}
}

// fillNodeCache records later builds' cache entries, which is the pressure
// that makes the node cap delete anything at all.
func fillNodeCache(t *testing.T, st store.Store, jobName string, count int) {
	t.Helper()

	for i := range count {
		mustRecordNode(t, st, jobName, hashOf(i+1))
	}
}

// TestPruneKeepsTheNewestRuns pins WHICH runs survive, and that the cap is
// per job: one busy job must not evict a quiet one's only run.
func (s suite) TestPruneKeepsTheNewestRuns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for n := 1; n <= 8; n++ {
		build(ctx, t, st, "busy", fmt.Sprintf("RUN%05d", n), n)
	}

	build(ctx, t, st, "quiet", "QUIET", 99)

	err := st.Prune(ctx, store.Retention{JobName: "busy", Runs: 3}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got, want := runIDs(t, st, "busy"), []string{"RUN00006", "RUN00007", "RUN00008"}; !slices.Equal(got, want) {
		t.Errorf("kept %v, want %v", got, want)
	}

	if got := runIDs(t, st, "quiet"); len(got) != 1 {
		t.Errorf("the quiet job kept %v, want its one run — the cap is global, not per job", got)
	}
}

// TestPruneIsSafeOnAnEmptyDatabase: retention runs at the end of every build,
// so it meets a job with nothing recorded constantly — and zero is no limit.
func (s suite) TestPruneIsSafeOnAnEmptyDatabase(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	err := st.Prune(ctx, store.Retention{JobName: "nothing-here", Runs: 10}, "")
	if err != nil {
		t.Fatalf("Prune on an empty database: %v", err)
	}

	build(ctx, t, st, "job", "ONLY", 1)

	err = st.Prune(ctx, store.Retention{JobName: "job", Runs: 0}, "")
	if err != nil {
		t.Fatalf("Prune with no cap: %v", err)
	}

	if got := runIDs(t, st, "job"); len(got) != 1 {
		t.Errorf("runs = %v after an uncapped prune, want the one — zero must mean no limit", got)
	}
}

// TestPruneKeepsTheRunItWasCalledFrom: a resumed run keeps its original
// started_at, so resuming an old run makes it the OLDEST row — and retention
// at the end of its own build deleted it, cascading away the completed steps
// a further resume reads.
func (s suite) TestPruneKeepsTheRunItWasCalledFrom(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	for n := 1; n <= 4; n++ {
		build(ctx, t, st, "job", fmt.Sprintf("RUN%05d", n), n)
	}

	const resumed = "RUN00001"

	err := st.ResumeRun(ctx, resumed, "/tmp/ws", "")
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}

	err = st.Prune(ctx, store.Retention{JobName: "job", Runs: 3}, resumed)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	assertRunSurvives(t, st, resumed, "the run being resumed was deleted by its own prune")

	steps, err := st.CompletedRunSteps(ctx, resumed)
	if err != nil {
		t.Fatalf("CompletedRunSteps: %v", err)
	}

	if len(steps) == 0 {
		t.Error("the resumed run kept its row but lost its completed steps to the cascade")
	}
}

// TestPruneSparesARunStillInFlight: a second build of one job, older and still
// running, must not be reaped by the one that finishes first — its own later
// inserts would then fail their foreign keys.
func (s suite) TestPruneSparesARunStillInFlight(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	mustStartRuns(ctx, t, st, "job", "INFLIGHT")

	for n := 1; n <= 4; n++ {
		build(ctx, t, st, "job", fmt.Sprintf("RUN%05d", n), n)
	}

	err := st.Prune(ctx, store.Retention{JobName: "job", Runs: 2}, "")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	assertRunSurvives(t, st, "INFLIGHT", "a run still in flight was deleted by another build's prune")
}

// TestPruneKeepsWhatASurvivingRunPointsAt: placements and usage cascade off
// nodes, and a FAILED step's step_finished carries no hash — so a node cap
// that exempted only what events name kept a retained run's green records
// and lost exactly its red ones.
func (s suite) TestPruneKeepsWhatASurvivingRunPointsAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const (
		runID   = "FAILED01"
		jobName = "build"
		keep    = 1
	)

	mustStartRuns(ctx, t, st, jobName, runID)

	placedHash, agentHash := hashOf(900_001), hashOf(900_002)

	for index, hash := range []string{placedHash, agentHash} {
		err := st.RecordNode(ctx, store.NodeRecord{
			Hash: hash, Kind: "task", StepIndex: index, Content: map[string]any{"body": index},
		}, jobName, "failed", nil, errors.New("boom"))
		if err != nil {
			t.Fatalf("RecordNode: %v", err)
		}

		err = st.AppendRunEvent(ctx, store.RunEventRow{
			RunID: runID, Type: "step_finished", StepIndex: index, Status: "failed", At: time.Now(),
		})
		if err != nil {
			t.Fatalf("AppendRunEvent: %v", err)
		}
	}

	mustPlace(t, st, store.Placement{
		RunID: runID, StepName: "unit", JobName: jobName, NodeHash: placedHash, Slot: placedHash,
		Tag: "spot", Address: "aws://i-0123456789abcdef0", GOOS: "linux", GOARCH: "arm64",
	})

	err := st.RecordAgentUsage(ctx, store.AgentUsage{
		RunID: runID, StepIndex: 1, StepName: "review", JobName: jobName, NodeHash: agentHash,
		ModelReq: "haiku", Total: 1_100, FinishReason: "error",
	})
	if err != nil {
		t.Fatalf("RecordAgentUsage: %v", err)
	}

	fillNodeCache(t, st, jobName, keep*nodesPerRetainedRun+5)

	err = st.Prune(ctx, store.Retention{JobName: jobName, Runs: keep}, runID)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	assertRunSurvives(t, st, runID, "the run itself was reaped, so this proves nothing about its records")

	placements, err := st.RunPlacements(ctx, runID)
	if err != nil || len(placements) != 1 {
		t.Errorf("a retained run reports %d placements (%v), want 1 — the node cap reaped where its failed step ran", len(placements), err)
	}

	usage, err := st.RunUsage(ctx, runID)
	if err != nil || len(usage) != 1 {
		t.Errorf("a retained run reports %d usage rows (%v), want 1 — the node cap reaped what its failed agent step spent", len(usage), err)
	}
}

// TestPruneStillWorksBesideAHookPlacement: a hook's placement has no node, and
// one NULL in a `NOT IN` list makes the clause match nothing — the node cache
// would stop being pruned forever, silently.
func (s suite) TestPruneStillWorksBesideAHookPlacement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const (
		runID   = "HOOKRUN1"
		jobName = "build"
		keep    = 1
	)

	mustStartRuns(ctx, t, st, jobName, runID)

	mustPlace(t, st, store.Placement{
		RunID: runID, StepName: "build", JobName: jobName,
		Slot: `step 0 (task "build") (on_failure hook)`,
		Tag:  "spot", Address: "aws://i-0123456789abcdef0", GOOS: "linux", GOARCH: "arm64",
	})

	fillNodeCache(t, st, jobName, keep*nodesPerRetainedRun+40)

	before := countNodes(t, st, jobName)

	err := st.Prune(ctx, store.Retention{JobName: jobName, Runs: keep}, runID)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	after := countNodes(t, st, jobName)

	if after >= before {
		t.Errorf("nodes went %d -> %d beside a hook placement; the cache is no longer bounded at all", before, after)
	}

	if after > keep*nodesPerRetainedRun {
		t.Errorf("nodes = %d after pruning, want no more than the cap of %d", after, keep*nodesPerRetainedRun)
	}
}

// TestPruneNodesKeepsTheNewestAndWhatAnEventNames pins both halves of the
// node cap: the newest entries up to the cap survive, older ones go, and an
// older one survives anyway while a retained run's events name it.
func (s suite) TestPruneNodesKeepsTheNewestAndWhatAnEventNames(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := s.open(t, "test")

	const (
		jobName = "build"
		keep    = 1
		fill    = keep*nodesPerRetainedRun + 5
	)

	// The named node is recorded FIRST, so it is the oldest entry the cap sees.
	build(ctx, t, st, jobName, "NAMED001", 900_001)
	fillNodeCache(t, st, jobName, fill)

	named := hashOf(1_000_000 + 900_001)

	err := st.Prune(ctx, store.Retention{JobName: jobName, Runs: keep}, "NAMED001")
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got, want := countNodes(t, st, jobName), keep*nodesPerRetainedRun+1; got != want {
		t.Errorf("%d nodes after the prune, want %d: the newest %d plus the one the run's events name", got, want, keep*nodesPerRetainedRun)
	}

	found, err := st.NodesByHash(ctx, []string{named, hashOf(1), hashOf(fill)})
	if err != nil {
		t.Fatalf("NodesByHash: %v", err)
	}

	for hash, want := range map[string]bool{named: true, hashOf(1): false, hashOf(fill): true} {
		if _, ok := found[hash]; ok != want {
			t.Errorf("node %s… survived = %v, want %v", hash[len(hash)-8:], ok, want)
		}
	}
}
