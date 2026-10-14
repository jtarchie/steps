package pipeline

// Exclusive pools (#205) end to end through RunJob, on the borrowable venue: a launch rung whose fake acquirer hands out this machine as a local: worker.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/testdocker"
	"github.com/jtarchie/steps/internal/venue"
)

const cappedPool = "aws://launch/lt-0def4567890abcde?exclusive&max=1"

func notesOf(t *testing.T, st store.Store, runID string) string {
	t.Helper()

	rows, err := st.RunEvents(context.Background(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	var notes strings.Builder

	for _, row := range rows {
		if row.Type == events.TypeStepNote {
			notes.WriteString(row.Text + "\n")
		}
	}

	return notes.String()
}

// awaitNote waits for a run's transcript to say something.
func awaitNote(t *testing.T, st store.Store, runID, want string) {
	t.Helper()

	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(notesOf(t, st, runID), want); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the run's transcript = %q, want %q", notesOf(t, st, runID), want)
		}
	}
}

// The spine of #205: a job past max waits, says so, and is handed the first job's machine when it ends.
func TestAJobPastMaxWaitsForTheFirstJobsMachine(t *testing.T) {
	testdocker.Require(t)

	started := containerPath(t, "started")
	release := containerPath(t, "release")

	ctx, cfg, provider, st, fake := borrowedRunWith(t, fmt.Sprintf(`
jobs:
- name: long
  plan:
  - task: hold
    tags: [box]
    image: alpine:3
    run: |
      touch %s
      until [ -f %s ]; do sleep 0.05; done
- name: short
  plan:
  - task: quick
    tags: [box]
    image: alpine:3
    run: "true"
`, started, release), map[string]string{"box": cappedPool})

	longCtx, stopLong := context.WithCancel(ctx)
	t.Cleanup(stopLong)

	long := make(chan error, 1)

	go func() { long <- RunJob(longCtx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) }()

	holder := awaitInContainer(t, started)

	runID := NewRunID()
	short := make(chan error, 1)

	go func() { short <- RunJob(ctx, cfg, &cfg.Jobs[1], st, Run{ID: runID, Provider: provider}) }()

	awaitNote(t, st, runID, "waiting for one")

	select {
	case err := <-short:
		t.Fatalf("the short job ran beside the long one on a pool of one: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	touchInContainer(t, holder, release)

	err := <-long
	if err != nil {
		t.Fatalf("long job: %v", err)
	}

	select {
	case err := <-short:
		if err != nil {
			t.Fatalf("short job, after waiting its turn: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the short job was never handed the machine")
	}

	if starts, stops := fake.counts(); starts != 1 || stops != 1 {
		t.Errorf("%d starts, %d stops: want one machine handed from job to job, given back by the last", starts, stops)
	}
}

// A step whose timeout: is shorter than its wait still runs: the turn is taken before the step's clock starts. A fake runner, so the step's own run is milliseconds and only the wait could spend the second.
func TestAStepsTimeoutDoesNotCountItsWaitForAPoolMachine(t *testing.T) {
	testdocker.Require(t)

	previous := newRunner
	newRunner = func(spec shell.RunnerSpec) (shell.Runner, error) {
		if spec.Worker == "" {
			return previous(spec)
		}

		return onceEvicted{}, nil
	}

	t.Cleanup(func() { newRunner = previous })

	ctx, cfg, provider, st, _ := borrowedRunWith(t, `
jobs:
- name: build
  plan:
  - task: work
    tags: [box]
    image: alpine:3
    timeout: 1s
    run: "true"
`, map[string]string{"box": cappedPool})

	registry, _ := ctx.Value(registryKey{}).(*venue.Registry)
	holder := registry.Leases(workersFrom(ctx))

	_, err := holder.Resolve(ctx, "box")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	done := make(chan error, 1)

	go func() { done <- RunJob(ctx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) }()

	select {
	case err := <-done:
		t.Fatalf("the job ran while the pool's only machine was held: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}

	err = holder.ReleaseAll(ctx)
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a step that waited past its timeout: = %v, want it run", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the job never ran")
	}
}

// onceEvicted is a placed step's first machine reclaimed under its first command, and every later one doing the work.
type onceEvicted struct {
	shell.Runner

	evict bool
}

func (o onceEvicted) Exec(context.Context, shell.Command) (shell.Result, error) {
	if o.evict {
		return shell.Result{}, fmt.Errorf("%w (EC2 spot terminate)", venue.ErrEvicted)
	}

	return shell.Result{}, nil
}

func (o onceEvicted) WithLabel(string) shell.Runner { return o }
func (onceEvicted) Close() error                    { return nil }

// A re-placement is refused once the step has spent its timeout: budget, and the turn it waited on a capped pool must not be counted as spent.
func TestAPoolWaitIsNotSpentFromAReplacedStepsBudget(t *testing.T) {
	testdocker.Require(t)

	var placed atomic.Int32

	previous := newRunner
	newRunner = func(spec shell.RunnerSpec) (shell.Runner, error) {
		if spec.Worker == "" {
			return previous(spec)
		}

		return onceEvicted{evict: placed.Add(1) == 1}, nil
	}

	t.Cleanup(func() { newRunner = previous })

	ctx, cfg, provider, st, _ := borrowedRunWith(t, `
jobs:
- name: build
  plan:
  - task: work
    tags: [box]
    image: alpine:3
    timeout: 1s
    run: "true"
`, map[string]string{"box": cappedPool})

	registry, _ := ctx.Value(registryKey{}).(*venue.Registry)
	holder := registry.Leases(workersFrom(ctx))

	_, err := holder.Resolve(ctx, "box")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	done := make(chan error, 1)

	go func() { done <- RunJob(ctx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) }()

	time.Sleep(1500 * time.Millisecond)

	err = holder.ReleaseAll(ctx)
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a step evicted after waiting its turn = %v, want it re-placed and run", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the job never ran")
	}

	if got := placed.Load(); got != 2 {
		t.Errorf("%d runners on the pool, want the evicted one and its replacement", got)
	}
}

// A poll's check rides along on the machine a job holds, as Concourse exempts checks from limit-active-tasks, rather than queueing behind the job.
func TestAPollRidesAlongOnTheMachineAJobHolds(t *testing.T) {
	t.Parallel()

	ctx, _, _, _, fake := borrowedRunWith(t, "jobs: []\n", map[string]string{"box": cappedPool})

	jobCtx, releaseJob := jobLeases(ctx)
	t.Cleanup(func() { releaseJob(context.Background()) })

	_, err := leasesFrom(jobCtx).Resolve(jobCtx, "box")
	if err != nil {
		t.Fatalf("the job's Resolve: %v", err)
	}

	pollCtx, releasePoll := CheckScope(ctx)
	defer releasePoll()

	resolved := make(chan error, 1)

	go func() {
		_, err := leasesFrom(pollCtx).Resolve(pollCtx, "box")
		resolved <- err
	}()

	select {
	case err := <-resolved:
		if err != nil {
			t.Fatalf("the poll's Resolve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the poll queued behind the job holding the pool's only machine")
	}

	if starts, _ := fake.counts(); starts != 1 {
		t.Errorf("%d machines, want the poll on the job's", starts)
	}
}

// A job aborted while it waits stops waiting, and leaves nothing behind for the next job to queue behind.
func TestAJobAbortedWhileWaitingForAPoolMachineStops(t *testing.T) {
	testdocker.Require(t)

	started := containerPath(t, "started")
	release := containerPath(t, "release")

	ctx, cfg, provider, st, _ := borrowedRunWith(t, fmt.Sprintf(`
jobs:
- name: long
  plan:
  - task: hold
    tags: [box]
    image: alpine:3
    run: |
      touch %s
      until [ -f %s ]; do sleep 0.05; done
- name: short
  plan:
  - task: quick
    tags: [box]
    image: alpine:3
    run: "true"
`, started, release), map[string]string{"box": cappedPool})

	longCtx, stopLong := context.WithCancel(ctx)
	t.Cleanup(stopLong)

	long := make(chan error, 1)

	go func() { long <- RunJob(longCtx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) }()

	holder := awaitInContainer(t, started)

	abortCtx, abort := context.WithCancelCause(ctx)
	aborted := make(chan error, 1)

	go func() { aborted <- RunJob(abortCtx, cfg, &cfg.Jobs[1], st, Run{Provider: provider}) }()

	time.Sleep(200 * time.Millisecond)
	abort(errors.New("aborted on request"))

	select {
	case err := <-aborted:
		if err == nil {
			t.Fatal("a job aborted while waiting reported success")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a job aborted while waiting kept waiting")
	}

	touchInContainer(t, holder, release)

	err := <-long
	if err != nil {
		t.Fatalf("long job: %v", err)
	}

	within := make(chan error, 1)

	go func() { within <- RunJob(ctx, cfg, &cfg.Jobs[1], st, Run{Provider: provider}) }()

	select {
	case err := <-within:
		if err != nil {
			t.Fatalf("the next job: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the aborted waiter left the pool's machine to nobody")
	}
}

// Job 1 holding a and waiting for b while job 2 holds b and waits for a can never end, so a job touching two capped pools is refused before it runs.
func TestAJobTouchingTwoCappedPoolsIsRefused(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`
resource_types:
- name: probe
  image: alpine:3
  config:
    check: printf '[{"ref":"v1"}]'
    in: "true"
resources:
- {name: src, type: probe, tags: [c], source: {}}
jobs:
- name: both
  plan:
  - {task: one, tags: [a], image: alpine:3, run: "true"}
  - {task: two, tags: [b], image: alpine:3, run: "true"}
- name: through-a-resource
  plan:
  - {task: one, tags: [a], image: alpine:3, run: "true"}
  - get: src
- name: one-pool-two-tags
  plan:
  - {task: one, tags: [a], image: alpine:3, run: "true"}
  - {task: two, tags: [a2], image: alpine:3, run: "true"}
- name: capped-and-uncapped
  plan:
  - {task: one, tags: [a], image: alpine:3, run: "true"}
  - {task: two, tags: [open], image: alpine:3, run: "true"}
  - {task: three, tags: [shared], image: alpine:3, run: "true"}
`), "pools", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	ctx, err := WithWorkers(context.Background(), map[string]string{
		"a":      "aws://launch/lt-0aaa4567890abcde?exclusive&max=2",
		"a2":     "aws://launch/lt-0aaa4567890abcde/var/tmp?max=2&exclusive",
		"b":      "aws://launch/lt-0bbb4567890abcde?exclusive&max=1",
		"c":      "gcp://launch/tmpl?zone=us-central1-a&exclusive&max=3",
		"open":   "aws://launch/lt-0bbb4567890abcde?exclusive",
		"shared": "aws://launch/lt-0bbb4567890abcde",
	})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	err = ValidateExclusivePools(ctx, cfg)
	if err == nil {
		t.Fatal("two jobs that each touch two capped pools were accepted")
	}

	for _, want := range []string{`job "both"`, "a, b", `job "through-a-resource"`, "a, c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, want it to name %s", err, want)
		}
	}

	for _, accepted := range []string{"one-pool-two-tags", "capped-and-uncapped"} {
		if strings.Contains(err.Error(), accepted) {
			t.Errorf("refusal = %q, names %s, which can never wait on itself", err, accepted)
		}
	}
}
