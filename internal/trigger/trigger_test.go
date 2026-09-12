package trigger

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
	"github.com/jtarchie/steps/internal/workspace"
)

// captureMu serializes captures against each other; see captureStdout.
//
//nolint:gochecknoglobals // one capture at a time, for one process-wide destination
var captureMu sync.Mutex

// captureStdout reads what fn printed through this package's own output
// destination.
//
// It used to assign the os.Stdout GLOBAL, which the package's parallel tests
// read concurrently through every fmt.Printf in the code under test — a data
// race -race reported intermittently. Swapping trigger's own writer under its
// lock is the same capture without the race; see output.go.
//
// The swap is released BEFORE fn runs: printf takes the read lock, and an
// RWMutex is not reentrant, so holding the write lock across fn would deadlock
// on the first line it tried to print.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	// One capture at a time. The lock above makes the SWAP safe; it does not
	// make two captures independent, because there is one destination to swap.
	// Two parallel tests capturing at once both point `out` at their own pipe,
	// the second swap wins, and the first test's own line is delivered to the
	// second test's pipe — so the first fails saying it never printed
	// something it did print.
	//
	// Serializing captures is enough because it is only the CAPTURING tests
	// that must not overlap: a test that merely prints has nothing to lose,
	// and its lines landing in someone's capture is the interleaving that
	// TestCaptureDoesNotRaceConcurrentOutput documents as acceptable.
	captureMu.Lock()
	defer captureMu.Unlock()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	outMu.Lock()
	orig := out
	out = w
	outMu.Unlock()

	// Drained WHILE fn runs, not after. printf holds outMu.RLock across its
	// Fprintf, so a writer that fills the pipe's 64KiB buffer blocks holding
	// the read lock — and the restore below takes the write lock, which then
	// never acquires. w.Close() would unstick it, but it is sequenced after
	// the restore, so the deadlock is permanent and takes the whole package
	// out to the go-test timeout.
	captured := make(chan []byte, 1)

	go func() {
		data, _ := io.ReadAll(r)
		captured <- data
	}()

	fn()

	outMu.Lock()
	out = orig
	outMu.Unlock()

	_ = w.Close()

	return string(<-captured)
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

func mustOpenStore(t *testing.T, dir string) store.Store {
	t.Helper()

	st, err := sqlite.OpenStore(filepath.Join(dir, ".steps", "state.db"), "test")
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

// TestAffectedJobsReachesNestedTrigger proves AffectedJobs finds a
// trigger:true get nested inside an in_parallel: branch — the same depth
// Resources (via config.PolledResourceNames) already reaches. Before
// config.Job.TriggersOn, AffectedJobs scanned job.Plan flatly: the poller
// would check and advance resource_checks for "nested" on every version
// change, yet no job would ever be enqueued for it, because the top-level
// step in job.Plan is the in_parallel: container itself (GetResourceName()
// == "").
func TestAffectedJobsReachesNestedTrigger(t *testing.T) {
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
- name: nested
  type: dummy
  source: {}
jobs:
- name: build
  plan:
  - in_parallel:
      steps:
      - get: nested
        trigger: true
`)

	jobs := AffectedJobs(cfg, "nested")

	names := make([]string, len(jobs))
	for i, j := range jobs {
		names[i] = j.Name
	}

	want := []string{"build"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("AffectedJobs names = %v, want %v — a trigger:true get nested in an in_parallel: branch must still enqueue its job", names, want)
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

// The line is the operator's only sign a poll enqueued anything.
func TestPollAndLogSaysWhatItEnqueued(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	versionsPath := filepath.Join(dir, "versions.json")
	writeVersions(t, versionsPath, `[{"ref":"v1"}]`)

	cfg := loadConfig(t, dir, dummyPipeline(versionsPath, filepath.Join(dir, "task-counter.txt")))
	st := mustOpenStore(t, dir)

	printed := captureStdout(t, func() { pollAndLog(context.Background(), cfg, st) })
	if !strings.Contains(printed, "trigger: enqueued build\n") {
		t.Errorf("printed %q, want the job the poll enqueued named", printed)
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

	err = Poll(context.Background(), staticConfig(cfg), st, 0)
	if err == nil {
		t.Fatal("Poll: expected an error for a non-positive interval, not a ticker panic")
	}
}

// staticConfig is a ConfigSource for a test whose configuration never
// changes, which is every test but the reload one.
func staticConfig(cfg *config.Config) ConfigSource {
	return func() *config.Config { return cfg }
}
