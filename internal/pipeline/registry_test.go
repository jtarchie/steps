package pipeline

// A borrowable venue that is not a cloud: acquiring it hands out this machine as a local: worker, and the counts are the point.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/venue"
	"github.com/jtarchie/steps/internal/workspace"
)

// borrowedWorker is a parked instance as far as every check is concerned; ?shim= satisfies the aws placement check, and the fake acquirer means nothing ever dials AWS.
const borrowedWorker = "aws://stopped/i-0abc123def456789?shim=/usr/local/bin/steps"

type borrowed struct {
	mu     sync.Mutex
	starts int
	stops  int
	// deadStops counts give-backs attempted on a context already cancelled. The real EC2 and GCE calls abort before reaching the API on one, so each is a machine left billing.
	deadStops int
	failStops bool
}

func (b *borrowed) acquire(context.Context, venue.Worker) (venue.Worker, func(context.Context) error, error) {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()

	return venue.Worker{URL: "local:", Scheme: venue.SchemeLocal}, func(ctx context.Context) error {
		b.mu.Lock()
		defer b.mu.Unlock()

		if ctx.Err() != nil {
			b.deadStops++

			return ctx.Err()
		}

		if b.failStops {
			return errors.New("the instance refused to stop")
		}

		b.stops++

		return nil
	}, nil
}

func (b *borrowed) counts() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.starts, b.stops
}

// borrowedRun is a pipeline, a store, a workspace and a context whose box tag names a borrowed machine.
func borrowedRun(t *testing.T, yaml string) (context.Context, *config.Config, workspace.Provider, store.Store, *borrowed) {
	t.Helper()

	// A registry bypassed by mistake acquires through the real EC2 client, and that must fail here rather than reach whatever account the shell has.
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	err := os.WriteFile(path, []byte(yaml), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = provider.Close() })

	st, err := sqlite.OpenStore(filepath.Join(dir, "state.db"), "test")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	ctx, err := WithWorkers(context.Background(), map[string]string{"box": borrowedWorker})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	fake := &borrowed{}

	ctx, closeRegistry := withRegistry(ctx, venue.NewRegistryWith(fake.acquire))
	t.Cleanup(closeRegistry)

	return ctx, cfg, provider, st, fake
}

// The failure the registry exists for, end to end through RunJob: the job that finishes first used to stop the machine the other was mid-step on.
func TestTwoJobsShareOneBorrowedMachine(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")

	ctx, cfg, provider, st, fake := borrowedRun(t, fmt.Sprintf(`
jobs:
- name: long
  plan:
  - task: hold
    tags: [box]
    run: |
      touch %s
      until [ -f %s ]; do sleep 0.05; done
- name: short
  plan:
  - task: quick
    tags: [box]
    run: "true"
`, started, release))

	// However this test ends, the long job has to be let go or its goroutine outlives it.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	long := make(chan error, 1)

	go func() { long <- RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) }()

	awaitFile(t, started)

	err := RunJob(ctx, cfg, &cfg.Jobs[1], nil, provider, st, false)
	if err != nil {
		t.Fatalf("short job: %v", err)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 0 {
		t.Fatalf("after the short job: %d starts, %d stops — want one machine, still up under the long job", starts, stops)
	}

	err = os.WriteFile(release, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = <-long
	if err != nil {
		t.Fatalf("long job: %v", err)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 1 {
		t.Fatalf("after both jobs: %d starts, %d stops — want it given back once, by the last user", starts, stops)
	}
}

// The #106/#103 seam: an abort cancels RunJob's context mid-step, exactly as web's LocalRunner.Abort does (a WithCancelCause over the drain, cancelled with a cause), and the borrowed machine must still be given back once. The likeliest reason a give-back runs is that the caller's context was just cancelled, so a release riding it never reaches the API.
func TestAnAbortedJobGivesBackItsBorrowedMachine(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	ctx, cfg, provider, st, fake := borrowedRun(t, fmt.Sprintf(`
jobs:
- name: long
  plan:
  - task: hold
    tags: [box]
    run: |
      touch %s
      while true; do sleep 0.05; done
`, started))

	runCtx, abort := context.WithCancelCause(ctx)
	t.Cleanup(func() { abort(nil) })

	done := make(chan error, 1)

	go func() { done <- RunJob(runCtx, cfg, &cfg.Jobs[0], nil, provider, st, false) }()

	awaitFile(t, started)

	if starts, stops := fake.counts(); starts != 1 || stops != 0 {
		t.Fatalf("mid-step: %d starts, %d stops — want the machine up under the job", starts, stops)
	}

	abort(errors.New("aborted on request"))

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RunJob returned nil for a job aborted mid-step")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("RunJob did not return after the abort")
	}

	fake.mu.Lock()
	starts, stops, dead := fake.starts, fake.stops, fake.deadStops
	fake.mu.Unlock()

	if dead != 0 {
		t.Errorf("%d give-backs ran on the aborted context; a real cloud call would never have reached the API", dead)
	}

	if starts != 1 || stops != 1 {
		t.Errorf("after the abort: %d starts, %d stops — want the machine given back exactly once", starts, stops)
	}
}

// A job's pre-plan check on a borrowed machine runs; skipped, as it had to be while a check and a job could not share one, the second run builds the version history already had rather than the one upstream has now.
func TestAJobChecksAResourceOnABorrowedMachine(t *testing.T) {
	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	fetched := filepath.Join(dir, "fetched")

	ctx, cfg, provider, st, fake := borrowedRun(t, fmt.Sprintf(`
resource_types:
- name: probe
  config:
    check: printf '[{"ref":"%%s"}]' "$(cat %s)"
    in: echo {{ .version.ref }} >> %s

resources:
- name: repo
  type: probe
  tags: [box]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
`, upstream, fetched))

	// What a poll left behind: history reads v1, while upstream has since moved on. With no history at all the get checks for itself, and the refresh would not be what decides.
	_, err := st.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(upstream, []byte("v2"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	runID := NewRunID()

	err = RunJob(WithNewRun(ctx, runID), cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	// The acquisition alone proves nothing: runPlacedStage resolves the worker itself, so only a placement record says the stage ran there rather than here.
	if tag := placementTags(t, st, runID)["repo"]; tag != "box" {
		t.Errorf("the fetch was recorded on %q, want box", tag)
	}

	log, err := os.ReadFile(fetched) //nolint:gosec // a t.TempDir() file this test's pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(string(log)); got != "v2" {
		t.Errorf("fetched %q, want v2 — the check was skipped and the run built what history had", got)
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 1 {
		t.Errorf("%d starts, %d stops — want the check and the fetch sharing one machine, given back once", starts, stops)
	}

	err = ValidatePipelinePlacement(ctx, cfg, []string{"repo"})
	if err != nil {
		t.Errorf("a polled resource on a borrowed machine was refused: %v", err)
	}
}

func awaitFile(t *testing.T, path string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)

	for time.Now().Before(deadline) {
		_, err := os.Stat(path)
		if err == nil {
			return
		}

		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("%s never appeared", path)
}

// A machine that could not be given back bills until somebody notices, so the job's release and the process's each say so, and only when it happened.
func TestAWorkerThatCannotBeGivenBackIsReported(t *testing.T) {
	t.Setenv("AWS_ENDPOINT_URL", "http://127.0.0.1:1")

	for scope, c := range map[string]struct{ worker, warning string }{
		"job":     {borrowedWorker, "acquired for this job could not be released"},
		"process": {borrowedWorker + "&idle=1h", "acquired by this process could not be released"},
	} {
		for _, failing := range []bool{false, true} {
			ctx, err := WithWorkers(context.Background(), map[string]string{"box": c.worker})
			if err != nil {
				t.Fatalf("WithWorkers: %v", err)
			}

			fake := &borrowed{failStops: failing}
			ctx, closeRegistry := withRegistry(ctx, venue.NewRegistryWith(fake.acquire))
			ctx, release := WithLeases(ctx)

			_, err = leasesFrom(ctx).Resolve(ctx, "box")
			if err != nil {
				t.Fatalf("%s: Resolve: %v", scope, err)
			}

			out := captureStdout(t, func() {
				release(context.Background())
				closeRegistry()
			})

			if warned := strings.Contains(out, c.warning); warned != failing {
				t.Errorf("%s scope, stop failing=%v: warned=%v:\n%s", scope, failing, warned, out)
			}
		}
	}
}

func TestAPlacedStepIsRecordedWhereItRan(t *testing.T) {
	ctx, cfg, provider, st, _ := borrowedRun(t, `
jobs:
- name: build
  plan:
  - task: here
    run: "true"
  - task: there
    tags: [box]
    run: "true"
`)

	runID := NewRunID()

	err := RunJob(WithNewRun(ctx, runID), cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	placements, err := st.RunPlacements(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	if len(placements) != 1 || placements[0].StepName != "there" || placements[0].Tag != "box" {
		t.Fatalf("placements = %+v, want one, for the tagged step on box", placements)
	}

	if placements[0].InstanceID != nil {
		t.Errorf("a local: worker has no instance, yet one was recorded: %q", *placements[0].InstanceID)
	}

	workers := eventWorkers(t, st, runID)

	if slices.ContainsFunc(workers["here"], func(worker string) bool { return worker != "" }) {
		t.Errorf("the untagged step's events name workers %q", workers["here"])
	}

	if !slices.ContainsFunc(workers["there"], func(worker string) bool { return strings.HasPrefix(worker, "box (") }) {
		t.Errorf("no event of the tagged step says it ran on box: %q", workers["there"])
	}
}

// eventWorkers is every worker a run's events named, by step.
func eventWorkers(t *testing.T, st store.Store, runID string) map[string][]string {
	t.Helper()

	rows, err := st.RunEvents(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	workers := map[string][]string{}
	for _, row := range rows {
		workers[row.StepName] = append(workers[row.StepName], row.Worker)
	}

	return workers
}

// placementTags is the tag each placed step of a run recorded, by step.
func placementTags(t *testing.T, st store.Store, runID string) map[string]string {
	t.Helper()

	placements, err := st.RunPlacements(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}

	tags := map[string]string{}
	for _, one := range placements {
		tags[one.StepName] = one.Tag
	}

	return tags
}

// evictingRunner is a check's first machine, reclaimed on its first command; only RunCapture, WithLabel and Close are reached.
type evictingRunner struct {
	shell.Runner

	calls, closes int
}

func (e *evictingRunner) RunCapture(context.Context, string) ([]byte, error) {
	e.calls++

	return nil, errTestEvicted
}

func (e *evictingRunner) WithLabel(string) shell.Runner { return e }

func (e *evictingRunner) Close() error {
	e.closes++

	return nil
}

// The check stage labels its runner, runs one command and closes what it holds; after a re-placement that must be the machine the check ended on, so it is closed and recorded, rather than the dead one a second time.
func TestACheckReplacedMidCommandHandsTheStageTheMachineItEndedOn(t *testing.T) {
	ctx, _, _, _, fake := borrowedRun(t, `
jobs:
- name: build
  plan:
  - task: work
    run: "true"
`)

	ctx, release := WithLeases(ctx)
	defer release(context.Background())

	ctx, sink := withPlacementSink(ctx)

	first := &evictingRunner{}

	var stage shell.Runner = &checkRunner{
		Runner: first,
		step:   config.Step{Get: "repo", Tags: []string{"box"}},
		spec:   shell.RunnerSpec{Worker: "local:", WorkerTag: "box"},
	}

	stage = stage.WithLabel("probe check")

	out, err := stage.RunCapture(ctx, "echo fresh")
	closeErr := stage.Close()

	if err != nil || closeErr != nil {
		t.Fatalf("RunCapture: %v; Close: %v", err, closeErr)
	}

	if got := strings.TrimSpace(string(out)); got != "fresh" {
		t.Errorf("the check answered %q, want the fresh machine's answer", got)
	}

	if first.calls != 1 || first.closes != 1 {
		t.Errorf("the reclaimed machine ran %d commands and was closed %d times, want once each", first.calls, first.closes)
	}

	placement, ok := sink.taken()
	if !ok || placement.Tag != "box" {
		t.Errorf("the machine the check ended on was never closed, so never recorded: %+v", placement)
	}

	if starts, _ := fake.counts(); starts != 1 {
		t.Errorf("%d machines acquired, want one replacement", starts)
	}
}
