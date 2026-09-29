package pipeline

// A borrowable venue that is not a cloud: acquiring it hands out this machine as a local: worker, and the counts are the point.

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

	return borrowedRunWith(t, yaml, nil)
}

// borrowedRunWith adds tags of the test's own beside box.
func borrowedRunWith(t *testing.T, yaml string, extra map[string]string) (context.Context, *config.Config, workspace.Provider, store.Store, *borrowed) {
	t.Helper()

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

	mappings := map[string]string{"box": borrowedWorker}
	maps.Copy(mappings, extra)

	ctx, err := WithWorkers(context.Background(), mappings)
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

// preplanFixture is a resource on the borrowed machine whose history reads v1 while upstream has moved to v2, fetched here by a get that overrides its tag — so the fetch acquires nothing, and any acquisition is the pre-plan refresh's.
func preplanFixture(t *testing.T, trigger string) (context.Context, *config.Config, workspace.Provider, store.Store, *borrowed, string) {
	t.Helper()

	dir := t.TempDir()
	upstream := filepath.Join(dir, "upstream")
	fetched := filepath.Join(dir, "fetched")

	ctx, cfg, provider, st, fake := borrowedRunWith(t, fmt.Sprintf(`
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
    tags: [here]
%s
`, upstream, fetched, trigger), map[string]string{"here": "local:"})

	// What a poll left behind: history reads v1, while upstream has since moved on. With no history at all the get checks for itself, and the refresh would not be what decides.
	_, err := st.RecordVersions(context.Background(), "repo", []map[string]any{{"ref": "v1"}}, 0)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(upstream, []byte("v2"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return ctx, cfg, provider, st, fake, fetched
}

func fetchedRef(t *testing.T, fetched string) string {
	t.Helper()

	log, err := os.ReadFile(fetched) //nolint:gosec // a t.TempDir() file this test's pipeline wrote
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(string(log))
}

// The poller keeps a polled resource's history, so a job's pre-plan refresh of one on a machine acquired on demand would start that machine for nothing the job needs.
func TestAJobAcquiresNoMachineToRefreshAPolledResource(t *testing.T) {
	ctx, cfg, provider, st, fake, fetched := preplanFixture(t, "    trigger: true")

	var err error

	out := captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) })
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if starts, _ := fake.counts(); starts != 0 {
		t.Errorf("%d starts — want none: the only thing on box is a check the poller already makes", starts)
	}

	if got := fetchedRef(t, fetched); got != "v1" {
		t.Errorf("fetched %q, want v1 — what the poller last recorded", got)
	}

	versions, err := st.ResourceVersionsJSON(context.Background(), "repo")
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 1 {
		t.Errorf("history has %d versions, want only the poller's v1", len(versions))
	}

	if !strings.Contains(out, "not refreshing repo before the plan: worker box is acquired on demand") {
		t.Errorf("the skip was not said:\n%s", out)
	}
}

// Nothing polls this one, so the refresh is the only thing that ever moves its history: skipped, every run would build the first run's version forever.
func TestARefreshStillChecksAResourceNothingPolls(t *testing.T) {
	ctx, cfg, provider, st, fake, fetched := preplanFixture(t, "")

	err := RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if starts, _ := fake.counts(); starts != 1 {
		t.Errorf("%d starts, want the refresh's one", starts)
	}

	if got := fetchedRef(t, fetched); got != "v2" {
		t.Errorf("fetched %q, want v2 — the refresh was skipped with nothing else to keep history fresh", got)
	}
}

// A one-shot command has no poller, so nothing but the refresh would keep a polled resource's history fresh either.
func TestARefreshWithoutAPollerStillChecks(t *testing.T) {
	_, cfg, _, _, _, _ := preplanFixture(t, "    trigger: true")

	ctx, err := WithWorkers(context.Background(), map[string]string{"box": borrowedWorker})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	if tag, skip := refreshAcquires(ctx, cfg, "repo"); skip {
		t.Errorf("skipped the refresh on %s with no poller to keep the history", tag)
	}

	shared, done := withRegistry(ctx, venue.NewRegistryWith((&borrowed{}).acquire))
	t.Cleanup(done)

	if _, skip := refreshAcquires(shared, cfg, "repo"); !skip {
		t.Error("the same resource under a daemon was refreshed; the fixture no longer proves the difference is the poller")
	}
}

// Polling a resource on a borrowed machine is what #103 allowed: the poll shares the machine through the registry.
func TestAPolledResourceOnABorrowedMachineIsAllowed(t *testing.T) {
	ctx, cfg, _, _, _, _ := preplanFixture(t, "    trigger: true")

	err := ValidatePipelinePlacement(ctx, cfg, []string{"repo"})
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

// A placement row can only exist once its node does (run_placements references it), so one per case also proves the node-then-placement order on both outcomes.
func TestEveryPlacedLeafStepIsRecordedWhereItRanPassOrFail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, plan, step string
		fails            bool
	}{
		{name: "task passes", plan: "- {task: work, tags: [box], run: \"true\"}", step: "work"},
		{name: "task fails", plan: "- {task: work, tags: [box], run: \"false\"}", step: "work", fails: true},
		{name: "first get passes", plan: "- get: good", step: "good"},
		{name: "first get fails", plan: "- get: bad", step: "bad", fails: true},
		{name: "second get passes", plan: "- {get: seed, resource: good}\n  - get: good", step: "good"},
		{name: "second get fails", plan: "- {get: seed, resource: good}\n  - get: bad", step: "bad", fails: true},
		{name: "put passes", plan: "- put: good", step: "good"},
		{name: "put fails", plan: "- put: bad", step: "bad", fails: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx, cfg, provider, st, _ := borrowedRun(t, `
resource_types:
- name: probe
  config:
    check: printf '[{"ref":"v1"}]'
    in: "true"
    out: printf '{"ref":"v2"}'
- name: broken
  config:
    check: printf '[{"ref":"v1"}]'
    in: "false"
    out: "false"

resources:
- {name: good, type: probe, tags: [box], source: {}}
- {name: bad, type: broken, tags: [box], source: {}}

jobs:
- name: build
  plan:
  `+tc.plan+`
`)

			runID := NewRunID()

			err := RunJob(WithNewRun(ctx, runID), cfg, &cfg.Jobs[0], nil, provider, st, false)
			if (err != nil) != tc.fails {
				t.Fatalf("RunJob error = %v, want failure %v", err, tc.fails)
			}

			if tag, ok := placementTags(t, st, runID)[tc.step]; !ok || tag != "box" {
				t.Fatalf("placements = %v, want %q recorded on box", placementTags(t, st, runID), tc.step)
			}
		})
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

// deadWorker is a machine that will not answer: its shim binary is not there, so the dial itself fails.
const deadWorker = "local:?binary=/nonexistent/steps"

// sequenced hands out the machines it is given in order, the last one for every acquisition after, and counts what it gave back.
type sequenced struct {
	mu       sync.Mutex
	machines []string
	starts   int
	stops    int
}

func (s *sequenced) acquire(context.Context, venue.Worker) (venue.Worker, func(context.Context) error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	machine, err := venue.ParseWorker(s.machines[min(s.starts, len(s.machines)-1)])
	if err != nil {
		return venue.Worker{}, nil, fmt.Errorf("sequenced: %w", err)
	}

	s.starts++

	return machine, func(context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.stops++

		return nil
	}, nil
}

func (s *sequenced) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.starts, s.stops
}

// warmRegistry is a daemon's context whose box tag names worker, acquired through fake; warm leaves the first machine in its idle window, as a job before this one would have.
func warmRegistry(t *testing.T, worker string, fake *sequenced, warm bool) context.Context {
	t.Helper()

	ctx, err := WithWorkers(context.Background(), map[string]string{"box": worker})
	if err != nil {
		t.Fatalf("WithWorkers: %v", err)
	}

	ctx, closeRegistry := withRegistry(ctx, venue.NewRegistryWith(fake.acquire))
	t.Cleanup(closeRegistry)

	if warm {
		earlier, release := WithLeases(ctx)

		_, err = leasesFrom(earlier).Resolve(earlier, "box")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}

		release(context.Background())
	}

	return ctx
}

const warmTask = `
jobs:
- name: build
  plan:
  - task: work
    tags: [box]
    attempts: 3
    inputs: []
    run: "true"
`

// warmRungs are both acquisition rungs, since they re-place differently: a launched machine is retired and a fresh one launched, while a parked one is the same entry started again.
var warmRungs = map[string]string{ //nolint:gochecknoglobals // a test table
	"stopped": "aws://stopped/i-0abc123def456789?shim=/usr/local/bin/steps&idle=1h",
	"launch":  "aws://launch/lt-0def4567890abcde?shim=/usr/local/bin/steps&idle=1h",
}

// A machine that died inside its idle window was handed to the next job as if alive, and the job failed on a plain dial error while a fresh machine was one acquisition away.
func TestADeadWarmMachineIsReplacedOnce(t *testing.T) {
	for rung, worker := range warmRungs {
		t.Run(rung, func(t *testing.T) {
			fake := &sequenced{machines: []string{deadWorker, "local:"}}
			ctx := warmRegistry(t, worker, fake, true)
			cfg, provider, st := warmJob(t)

			var err error

			out := captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) })
			if err != nil {
				t.Fatalf("RunJob: %v", err)
			}

			starts, stops := fake.counts()
			if starts != 2 {
				t.Errorf("%d acquisitions, want the dead machine and one replacement", starts)
			}

			if rung == "launch" && stops != 1 {
				t.Errorf("%d give-backs, want the dead launched machine given back once", stops)
			}

			if strings.Contains(out, "attempt 2/3") {
				t.Errorf("the dead machine spent the step's attempts:\n%s", out)
			}
		})
	}
}

// The twins: only a machine reused from its idle window is presumed reclaimed, only once, and a broken machine acquired fresh is still the plain failure it always was.
func TestAMachineThatWillNotAnswerIsNotAlwaysAnEviction(t *testing.T) {
	for name, c := range map[string]struct {
		machines []string
		warm     bool
		starts   int
	}{
		"acquired fresh":            {machines: []string{deadWorker, "local:"}, starts: 1},
		"warm, and its replacement": {machines: []string{deadWorker}, warm: true, starts: 2},
	} {
		for rung, worker := range warmRungs {
			t.Run(name+"/"+rung, func(t *testing.T) {
				fake := &sequenced{machines: c.machines}
				ctx := warmRegistry(t, worker, fake, c.warm)
				cfg, provider, st := warmJob(t)

				var err error

				_ = captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) })
				if err == nil {
					t.Fatal("RunJob succeeded on a machine that never answered")
				}

				if starts, _ := fake.counts(); starts != c.starts {
					t.Errorf("%d acquisitions, want %d", starts, c.starts)
				}
			})
		}
	}
}

func warmJob(t *testing.T) (*config.Config, workspace.Provider, store.Store) {
	t.Helper()

	_, cfg, provider, st, _ := borrowedRun(t, warmTask)

	return cfg, provider, st
}
