package trigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// captureStdout runs fn with os.Stdout redirected to a pipe, returning
// everything fn wrote via fmt.Printf and friends. Not safe alongside other
// tests running in parallel that also touch os.Stdout — callers must not use
// t.Parallel().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()

	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

// loadConfig writes yaml to a pipeline.yml under dir and parses it.
func loadConfig(t *testing.T, dir, yaml string) *config.Config {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatalf("write pipeline: %v", err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	return cfg
}

func mustOpenStore(t *testing.T, dir string) *store.Store {
	t.Helper()

	st, err := store.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}

	t.Cleanup(func() { _ = st.Close() })

	return st
}

func writeVersions(t *testing.T, path, json string) {
	t.Helper()

	err := os.WriteFile(path, []byte(json), 0o600)
	if err != nil {
		t.Fatalf("write versions file %q: %v", path, err)
	}
}

func TestResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: thing-a
  type: dummy
  source: {}
- name: thing-b
  type: dummy
  source: {}
- name: untriggered
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing-a
    trigger: true
  - get: thing-b
    trigger: true
  - get: untriggered
- name: other
  plan:
  - get: thing-a
    trigger: true
`)

	got := Resources(cfg)
	want := []string{"thing-a", "thing-b"}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resources = %v, want %v", got, want)
	}
}

func TestAffectedJobs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
- name: other
  plan:
  - get: thing
- name: also-build
  plan:
  - get: thing
    trigger: true
  - task: extra
    inputs: []
    run: echo hi
`)

	jobs := AffectedJobs(cfg, "thing")

	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}

	want := []string{"build", "also-build"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("AffectedJobs names = %v, want %v", names, want)
	}
}

// TestResourcesAndAffectedJobsResolveGetAlias confirms a get: aliasing its
// resource is polled and matched by the RESOLVED resource name, not the alias
// — so two aliases of one resource poll it once and both jobs are affected.
//
// TestConformance note: verifies steps's claim (internal/config/step.go's Step.Resource
// doc) that this mirrors Concourse's get.resource — see docs/conformance.md.
// Concourse doc: concourse-ci.org/docs/steps/get/.
func TestResourcesAndAffectedJobsResolveGetAlias(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []", in: "true", out: "true"}
resources:
- name: repo
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: source
    resource: repo
    trigger: true
- name: other
  plan:
  - get: mirror
    resource: repo
    trigger: true
`)

	got := Resources(cfg)
	if !reflect.DeepEqual(got, []string{"repo"}) {
		t.Errorf("Resources = %v, want [repo] (both aliases resolve to repo, deduped)", got)
	}

	jobs := AffectedJobs(cfg, "repo")

	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}

	if !reflect.DeepEqual(names, []string{"build", "other"}) {
		t.Errorf("AffectedJobs(repo) = %v, want [build other]", names)
	}
}

// dummyPipeline returns a pipeline with one trigger:true get step reading
// versions from versionsPath, and a task that appends to taskCounterPath
// each time it runs.
func dummyPipeline(versionsPath, taskCounterPath string) string {
	return fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: echo ran >> %s
`, versionsPath, taskCounterPath)
}

func TestPollOnceColdStartSeedsBaselineAndEnqueuesTheNewest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, filepath.Join(dir, "task-counter.txt")))
	st := mustOpenStore(t, dir)

	enqueued, err := pollOnce(context.Background(), cfg, st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	// One version exists, so there is no backlog below it: the cold start
	// builds it, the way Concourse builds what its first check reports.
	if len(enqueued) != 1 || enqueued[0] != "build" {
		t.Errorf("cold start enqueued %v, want [build] — the newest version", enqueued)
	}
}

func TestPollOnceEnqueuesOnVersionChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, filepath.Join(dir, "task-counter.txt")))
	st := mustOpenStore(t, dir)

	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// Unchanged: a second poll with the same latest version must not enqueue.
	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (unchanged): %v", err)
	}

	if len(enqueued) != 0 {
		t.Errorf("unchanged poll enqueued %v, want none", enqueued)
	}

	writeVersions(t, versionsPath, `[{"ref":"v1"},{"ref":"v2"}]`)

	enqueued, err = pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (changed): %v", err)
	}

	if !reflect.DeepEqual(enqueued, []string{"build"}) {
		t.Fatalf("changed poll enqueued %v, want [build]", enqueued)
	}
}

func TestPollOnceEnqueuesJobOnceForMultipleDirtyResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsA := filepath.Join(dir, "versions-a.json")
	versionsB := filepath.Join(dir, "versions-b.json")
	writeVersions(t, versionsA, `[{"ref":"v1"}]`)
	writeVersions(t, versionsB, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy-a
  config: {check: "cat %s"}
- name: dummy-b
  config: {check: "cat %s"}
resources:
- name: thing-a
  type: dummy-a
  source: {}
- name: thing-b
  type: dummy-b
  source: {}
jobs:
- name: build
  plan:
  - get: thing-a
    trigger: true
  - get: thing-b
    trigger: true
`, versionsA, versionsB))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// Both resources change before the next poll: "build" must still be
	// enqueued exactly once.
	writeVersions(t, versionsA, `[{"ref":"v1"},{"ref":"v2"}]`)
	writeVersions(t, versionsB, `[{"ref":"v1"},{"ref":"v2"}]`)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (both changed): %v", err)
	}

	if !reflect.DeepEqual(enqueued, []string{"build"}) {
		t.Fatalf("enqueued = %v, want exactly one [build]", enqueued)
	}
}

// TestPollOnceDoesNotConsumeChangeWhenLaterCheckFails is the at-least-once
// guarantee: a resource observed dirty must not have its recorded version
// advanced if a *later* resource's check fails in the same poll — otherwise
// the change would be silently consumed and the job never triggered.
// TestPollOnceSecondPollPassesLastVersionToCheck is the watch-loop half of
// the check cursor: the version recorded by one poll is what the NEXT poll's
// check renders against, so a type can ask its API for what it has not seen
// (Slack's oldest:, GitHub's since:) instead of guessing a window wide enough
// to cover the gap.
//
// The check here records what it was handed and returns a version derived
// from it, so the poll sequence is legible in one file: cold (no cursor),
// then the version the first poll recorded.
//
// TestConformance note: covers the watch half of the contract
// TestConformanceCheckReceivesCurrentVersion pins at the resource layer
// (concourse-ci.org/docs/resource-types/implementing/, "check" section).
func TestPollOnceSecondPollPassesLastVersionToCheck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.txt")

	// Each poll appends the cursor it saw, and returns a version naming the
	// poll number so the recorded cursor changes every time.
	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: |
      cursor='{{ index .version "ref" | default "cold" }}'
      echo "$cursor" >> %s
      count=$(wc -l < %s | tr -d ' ')
      printf '[{"ref": "v%%s"}]' "$count"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
`, seen, seen))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	for i := range 3 {
		_, err := pollOnce(ctx, cfg, st)
		if err != nil {
			t.Fatalf("pollOnce %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(seen) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
	if err != nil {
		t.Fatalf("read seen: %v", err)
	}

	got := strings.Fields(string(data))
	want := []string{"cold", "v1", "v2"}

	if !slices.Equal(got, want) {
		t.Errorf("cursors seen by successive checks = %v, want %v", got, want)
	}
}

func TestPollOnceDoesNotConsumeChangeWhenLaterCheckFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsA := filepath.Join(dir, "versions-a.json")
	versionsB := filepath.Join(dir, "versions-b.json")
	writeVersions(t, versionsA, `[{"ref":"v1"}]`)
	writeVersions(t, versionsB, `[{"ref":"v1"}]`)

	// thing-a is in the first job, so Resources() lists (and pollOnce checks)
	// it before thing-b: it is observed dirty before thing-b's error aborts.
	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy-a
  config: {check: "cat %s"}
- name: dummy-b
  config: {check: "cat %s"}
resources:
- name: thing-a
  type: dummy-a
  source: {}
- name: thing-b
  type: dummy-b
  source: {}
jobs:
- name: build-a
  plan:
  - get: thing-a
    trigger: true
- name: build-b
  plan:
  - get: thing-b
    trigger: true
`, versionsA, versionsB))

	st := mustOpenStore(t, dir)
	ctx := context.Background()

	_, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (baseline): %v", err)
	}

	// thing-a changes; thing-b's check now fails (its versions file is gone).
	writeVersions(t, versionsA, `[{"ref":"v1"},{"ref":"v2"}]`)

	err = os.Remove(versionsB)
	if err != nil {
		t.Fatalf("remove versions-b: %v", err)
	}

	_, err = pollOnce(ctx, cfg, st)
	if err == nil {
		t.Fatal("pollOnce: expected an error from the failing thing-b check")
	}

	// Heal thing-b (back to its unchanged baseline). thing-a's change must
	// still be pending — its version was not advanced by the failed poll.
	writeVersions(t, versionsB, `[{"ref":"v1"}]`)

	enqueued, err := pollOnce(ctx, cfg, st)
	if err != nil {
		t.Fatalf("pollOnce (healed): %v", err)
	}

	if !reflect.DeepEqual(enqueued, []string{"build-a"}) {
		t.Fatalf("enqueued = %v, want [build-a] (thing-a's change must survive the earlier failed poll)", enqueued)
	}
}

func TestDrainOneRunsClaimedJobAndReportsEmptyQueue(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)
	taskCounter := filepath.Join(dir, "task-counter.txt")

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, taskCounter))
	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	ctx := context.Background()

	err = st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, provider, st, nil, false)
	if err != nil {
		t.Fatalf("drainOne: %v", err)
	}

	if !ran {
		t.Fatal("drainOne: expected ran=true for a queued job")
	}

	assertTaskCounter(t, taskCounter, "ran\n")

	ran, err = drainOne(ctx, cfg, provider, st, nil, false)
	if err != nil {
		t.Fatalf("drainOne (empty queue): %v", err)
	}

	if ran {
		t.Fatal("drainOne: expected ran=false on an empty queue")
	}
}

// panicProvider is a workspace.Provider whose NewBuild always panics —
// used to prove drainOne recovers from a panic reached deep inside
// pipeline.RunJob instead of crashing the whole watch process.
type panicProvider struct{}

func (panicProvider) Validate() error { return nil }

func (panicProvider) NewBuild(context.Context, string) (workspace.BuildWorkspace, error) {
	panic("boom: simulated panic in workspace provider")
}

func (panicProvider) Close() error { return nil }

// TestDrainOneRecoversFromPanic confirms a panic inside pipeline.RunJob (here,
// forced via panicProvider.NewBuild) is recovered by drainOne rather than
// crashing the test process, the claimed job is finalized "failed" (not left
// stuck running/pending forever), and the returned error names the panic.
func TestDrainOneRecoversFromPanic(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, filepath.Join(dir, "task-counter.txt")))

	st := mustOpenStore(t, dir)

	ctx := context.Background()

	err := st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, panicProvider{}, st, nil, false)
	if !ran {
		t.Fatal("drainOne: expected ran=true after recovering from a panic on a claimed job")
	}

	if err == nil {
		t.Fatal("drainOne: expected a non-nil error after recovering from a panic")
	}

	if !strings.Contains(err.Error(), "recovered from panic") {
		t.Errorf("drainOne error = %v, want it to mention the recovered panic", err)
	}

	// The row must be completed (failed), not left claimed forever.
	_, _, found, claimErr := st.ClaimNextJob(ctx)
	if claimErr != nil {
		t.Fatalf("ClaimNextJob: %v", claimErr)
	}

	if found {
		t.Fatal("expected no claimable row after a recovered panic")
	}

	// A second drainOne call must proceed normally — the worker loop isn't
	// wedged by the earlier panic.
	ran, err = drainOne(ctx, cfg, panicProvider{}, st, nil, false)
	if ran || err != nil {
		t.Fatalf("drainOne (empty queue after recovery): ran=%v err=%v, want ran=false err=nil", ran, err)
	}
}

func TestWatchErrorsImmediatelyWithNoTriggerResources(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := loadConfig(t, dir, `
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config: {check: "echo []"}
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
`)

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	err = Watch(context.Background(), cfg, provider, st, nil, 0, 1, false, "")
	if err == nil {
		t.Fatal("Watch: expected an error when no get step sets trigger: true")
	}
}

// TestWatchRejectsNonPositiveInterval guards against a zero/negative
// --interval reaching time.NewTicker, which would panic instead of erroring.
func TestWatchRejectsNonPositiveInterval(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, filepath.Join(dir, "task-counter.txt")))

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	err = Watch(context.Background(), cfg, provider, st, nil, 0, 1, false, "")
	if err == nil {
		t.Fatal("Watch: expected an error for a non-positive interval, not a ticker panic")
	}
}

func TestDrainOneMarksGenuineFailureAsFailed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: exit 1
`, versionsPath))

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	ctx := context.Background()

	err = st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, provider, st, nil, false)
	if !ran || err == nil {
		t.Fatalf("drainOne: expected ran=true and a non-nil error for a failing task, got ran=%v err=%v", ran, err)
	}

	// The row must be completed (failed), not left running/pending, since
	// ctx was never canceled — this was a genuine failure.
	_, _, found, claimErr := st.ClaimNextJob(ctx)
	if claimErr != nil {
		t.Fatalf("ClaimNextJob: %v", claimErr)
	}

	if found {
		t.Fatal("expected no claimable row after a genuine (non-cancellation) failure")
	}
}

// TestDrainOneFailingJobWithOnFailureHookStillMarksFailed confirms hooks are
// observers even through the trigger worker: a job whose task fails and whose
// on_failure hook runs successfully is still recorded failed (drainOne returns
// a non-nil error and leaves no claimable row), not silently consumed.
func TestDrainOneFailingJobWithOnFailureHookStillMarksFailed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	marker := filepath.Join(dir, "notified.txt")

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: exit 1
    on_failure:
      task: notify
      run: echo notified >> %s
`, versionsPath, marker))

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	ctx := context.Background()

	err = st.EnqueueJob(ctx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	ran, err := drainOne(ctx, cfg, provider, st, nil, false)
	if !ran || err == nil {
		t.Fatalf("drainOne: expected ran=true and a non-nil error despite the on_failure hook, got ran=%v err=%v", ran, err)
	}

	// The on_failure hook must have fired.
	_, statErr := os.Stat(marker)
	if statErr != nil {
		t.Errorf("expected the on_failure hook to run and write %q, got %v", marker, statErr)
	}

	// The row must be completed (failed), not left claimable.
	_, _, found, claimErr := st.ClaimNextJob(ctx)
	if claimErr != nil {
		t.Fatalf("ClaimNextJob: %v", claimErr)
	}

	if found {
		t.Fatal("expected no claimable row after a genuine failure, even with an on_failure hook")
	}
}

// TestDrainOneLeavesCanceledJobReRunnableAfterSimulatedRestart is the
// graceful-shutdown carve-out: a job interrupted by ctx cancellation
// (SIGINT/SIGTERM mid-run) must not be marked failed — that would silently
// drop it, since only a new version change would otherwise ever re-trigger
// it. It must stay claimable after a simulated watch restart
// (ResetStaleRunning), and running it again to completion must succeed.
//
// The task's run: sleeps briefly so the claim (a fast DB op) completes
// before a short-timeout context cancels mid-sleep, the same shape a real
// SIGINT arriving mid-run has — as opposed to a context canceled before
// drainOne is even called, which would fail at the claim step itself and
// never reach RunJob at all.
func TestDrainOneLeavesCanceledJobReRunnableAfterSimulatedRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)
	taskCounter := filepath.Join(dir, "task-counter.txt")

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  # interruptible: true is what this test is ABOUT: it exercises the path
  # where a shutdown cancels a build mid-run. Jobs default to waiting
  # instead (Concourse's default), so the interruption has to be asked for.
  interruptible: true
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: sleep 1 && echo ran >> %s
`, versionsPath, taskCounter))

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	bgCtx := context.Background()

	err = st.EnqueueJob(bgCtx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bgCtx, 200*time.Millisecond)
	defer cancel()

	ran, err := drainOne(shortCtx, cfg, provider, st, nil, false)
	if !ran || err == nil {
		t.Fatalf("drainOne (interrupted mid-run): expected ran=true and a non-nil error, got ran=%v err=%v", ran, err)
	}

	// The 1s sleep should never have completed before the 200ms timeout hit.
	assertTaskCounter(t, taskCounter, "")

	// Simulate a watch restart: the row must still be there (running, not
	// failed) for ResetStaleRunning to recover, not silently dropped.
	err = st.ResetStaleRunning(bgCtx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	ran, err = drainOne(bgCtx, cfg, provider, st, nil, false)
	if err != nil {
		t.Fatalf("drainOne (after restart): %v", err)
	}

	if !ran {
		t.Fatal("drainOne (after restart): expected the interrupted job to still be claimable and re-run")
	}

	assertTaskCounter(t, taskCounter, "ran\n")
}

// TestDrainOneFixTaskInterruptedNeverInvokesFixAgent proves the accurate
// cancellation detection added to runFixTask end to end: a task with fix:
// interrupted mid-initial-run must be classified the same way a plain task
// is (drainOne returns an error satisfying errors.Is(_,
// context.DeadlineExceeded), and the row stays re-runnable, not "failed") —
// and, critically, must never invoke the fix agent, since the interrupted
// run was never a genuine failure verdict. The agent's endpoint points at an
// address nothing listens on, so if runFixTask incorrectly proceeded to
// invoke it, this test would fail with a connection error (or hang) instead
// of cleanly observing the interruption. Not parallel: uses t.Setenv.
func TestDrainOneFixTaskInterruptedNeverInvokesFixAgent(t *testing.T) {
	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

agents:
- name: fixer
  source:
    endpoint: http://127.0.0.1:1/v1/
    model: test-model
    api_key_env: STEPS_TEST_AGENT_API_KEY

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  # interruptible: true is what this test is ABOUT: it exercises the path
  # where a shutdown cancels a build mid-run. Jobs default to waiting
  # instead (Concourse's default), so the interruption has to be asked for.
  interruptible: true
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: sleep 1
    fix: fixer
`, versionsPath))

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test-key")

	st := mustOpenStore(t, dir)

	provider, err := workspace.NewProvider(cfg.Workspace, false)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	defer func() { _ = provider.Close() }()

	bgCtx := context.Background()

	err = st.EnqueueJob(bgCtx, "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(bgCtx, 200*time.Millisecond)
	defer cancel()

	// pipeline.go prints "invoking fix agent" via fmt.Printf to os.Stdout
	// immediately before calling agent.RunFix — capture it so the test proves
	// the fix agent was never reached, not merely that the final error happens
	// to satisfy errors.Is(_, DeadlineExceeded) (which an already-expired ctx
	// would also produce from deep inside a wrongly-attempted agent.RunFix
	// call, since its own HTTP client respects the same ctx — a weaker check
	// that wouldn't actually catch runFixTask skipping its cancellation guard).
	var ran bool

	stdout := captureStdout(t, func() {
		ran, err = drainOne(shortCtx, cfg, provider, st, nil, false)
	})

	if !ran || err == nil {
		t.Fatalf("drainOne (interrupted mid-run): expected ran=true and a non-nil error, got ran=%v err=%v", ran, err)
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("drainOne error = %v, want it to satisfy errors.Is(_, context.DeadlineExceeded)", err)
	}

	if strings.Contains(stdout, "invoking fix agent") {
		t.Errorf("drainOne invoked the fix agent for an interrupted run: stdout = %q", stdout)
	}

	// The row must still be recoverable as running (not "failed") — the same
	// contract a plain task's interruption gets.
	err = st.ResetStaleRunning(bgCtx)
	if err != nil {
		t.Fatalf("ResetStaleRunning: %v", err)
	}

	_, _, found, claimErr := st.ClaimNextJob(bgCtx)
	if claimErr != nil {
		t.Fatalf("ClaimNextJob: %v", claimErr)
	}

	if !found {
		t.Fatal("expected a re-queued row after ResetStaleRunning")
	}
}

// assertTaskCounter reads path (a test-owned temp file that may not exist
// yet) and fails the test unless its contents equal want.
func assertTaskCounter(t *testing.T, path, want string) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // test-owned temp file
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read task counter %q: %v", path, err)
	}

	if string(data) != want {
		t.Errorf("task counter = %q, want %q", data, want)
	}
}

// TestConformanceNonInterruptibleBuildSurvivesShutdown verifies interruptible:
// matches Concourse, including its default.
//
// Concourse doc: concourse-ci.org/docs/jobs/ — interruptible defaults to false,
// and when true "workers won't wait for this job's builds during shutdown".
// So the DEFAULT is that shutdown waits, which is what this pins: a deploy
// half-applied because someone restarted the watcher is the case the field
// exists for.
//
// The counterpart is TestDrainOneLeavesCanceledJobReRunnableAfterSimulatedRestart,
// which sets interruptible: true and asserts the opposite.
//
// Scoped to `steps watch`: `steps run` is a person at a terminal and ctrl-C
// there is always immediate. See config.Job.Interruptible.
func TestConformanceNonInterruptibleBuildSurvivesShutdown(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)
	marker := filepath.Join(dir, "finished.txt")

	// No interruptible: line at all — the default, which must WAIT.
	cfg := loadConfig(t, dir, fmt.Sprintf(`
defaults:
  preflight:
    disabled: true

resource_types:
- name: dummy
  config:
    check: cat %s
    in: "true"
resources:
- name: thing
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - get: thing
    trigger: true
  - task: work
    inputs: []
    run: sleep 1 && echo finished >> %s
`, versionsPath, marker))

	st := mustOpenStore(t, dir)

	err := st.EnqueueJob(context.Background(), "build", "thing")
	if err != nil {
		t.Fatalf("EnqueueJob: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	// Cancelled while the task is mid-sleep — the shutdown this is about.
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	_, err = drainOne(ctx, cfg, provider, st, nil, false)
	if err != nil {
		t.Errorf("drainOne returned %v, want nil — a non-interruptible build must be allowed to finish", err)
	}

	// The marker only exists if the task ran to completion despite the cancel.
	_, statErr := os.Stat(marker)
	if statErr != nil {
		t.Error("the build did not finish: a job that did not opt into interruptible: must survive a shutdown, or a deploy can be left half-applied")
	}
}

// TestBuildContextHasNoDeadlineUntilShutdown pins the difference between a
// shutdown grace and a job timeout.
//
// The first cut of buildContext spelled the grace as a plain WithTimeout on
// the detached context, which armed it at build START. Since not-interruptible
// is the default, that put a 10-minute ceiling on every build of every job —
// an ordinary 25-minute suite that had run fine the day before would be
// aborted with DeadlineExceeded, and nothing in the pipeline would explain it.
//
// The grace must be armed by the shutdown, so a build running while the
// watcher is healthy has no deadline of its own at all.
func TestBuildContextHasNoDeadlineUntilShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runCtx, release := buildContext(ctx, &config.Job{Name: "deploy"})
	defer release()

	deadline, ok := runCtx.Deadline()
	if ok {
		t.Errorf("a build under a healthy watcher has a deadline of %s; the grace must be armed by shutdown, not by build start", time.Until(deadline))
	}

	// And it must not inherit the watcher's cancellation either — that is the
	// whole point of not being interruptible.
	cancel()

	select {
	case <-runCtx.Done():
		t.Error("a non-interruptible build was cancelled the moment the watcher was")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestBuildContextInterruptibleSharesTheWatcherContext is the other half: a
// job that opted in dies with the watcher, immediately.
func TestBuildContextInterruptibleSharesTheWatcherContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runCtx, release := buildContext(ctx, &config.Job{Name: "report", Interruptible: true})
	defer release()

	cancel()

	select {
	case <-runCtx.Done():
	case <-time.After(time.Second):
		t.Error("an interruptible build outlived the watcher's cancellation")
	}
}
