package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/outcome"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/workspace"
)

// Conformance tests characterize a specific behavior steps claims (in a code
// comment or doc) to mirror Concourse, transcribed from Concourse's own docs
// or source at a pinned reference rather than by importing/vendoring its
// code (impractical: its version-selection/passed: logic is hard-coupled to
// Postgres even in Concourse's own unit tests). See docs/conformance.md for
// the full convention, the citation format each test below follows, and the
// living inventory of which "mirrors Concourse" claims in this repo are
// covered. Every conformance test is named TestConformance..., so
// `go test -run TestConformance ./...` runs the whole set.

// TestConformanceGetVersionEveryContinuesPastFailure verifies steps's
// get: version: every fan-out matches Concourse's version-selection cursor,
// which advances to the next version regardless of the prior version's
// build status.
//
// Concourse source: atc/db/versions_db.go, NextEveryVersion (the SQL query
// has no filter on build status) — github.com/concourse/concourse @ v8.2.4.
// Read directly from source, not from a written spec page; treat as a
// source-reading finding, not an official Concourse guarantee.
//
// steps claim under test: internal/config/step.go's Step.Version field
// doc, internal/resource/resource.go's VersionMode doc.
func TestConformanceGetVersionEveryContinuesPastFailure(t *testing.T) {
	dir := t.TempDir()
	taskCounter := filepath.Join(dir, "task-counter.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: printf '[{"ref":"v1"},{"ref":"v2"},{"ref":"v3"}]'
    in: echo {{ .version.ref | shellquote }} > ./ref

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
    version: every
  - task: work
    inputs: [thing]
    run: |
      ref=$(cat thing/ref)
      echo "ran $ref" >> %s
      test "$ref" != v2
`, taskCounter)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	job := &cfg.Jobs[0]

	runErr := RunJob(ctx, cfg, job, nil, provider, st, false)

	// All three versions must have been attempted in this one invocation —
	// under the bug this test guards against, only "ran v1" would appear
	// (v2 fails, v3 is never attempted).
	assertConformanceLineCount(t, taskCounter, 3)

	if runErr == nil {
		t.Fatal("RunJob returned nil; want a non-nil error since v2's build failed")
	}

	if got := outcome.Classify(ctx, runErr); got != outcome.Failed {
		t.Errorf("outcome.Classify(runErr) = %q, want %q", got, outcome.Failed)
	}

	// A second invocation runs NOTHING: v1 and v3 succeeded, and v2 — the one
	// that failed — has been taken all the same.
	//
	// This is the half that used to assert the opposite (that v2 is retried),
	// which was this repo's own interpretation rather than Concourse's.
	// NextEveryVersion picks the next version above the highest check_order in
	// build_resource_config_version_inputs — the versions a build was CREATED
	// with — and applies no filter on build status anywhere, so a version
	// consumed by a failed build is consumed. Re-running one is deliberate and
	// manual there (concourse/concourse#413) as it is here: --force, --resume,
	// or a new version.
	_ = RunJob(ctx, cfg, job, nil, provider, st, false)
	assertConformanceLineCount(t, taskCounter, 3)

	// And --force is that manual act: it re-runs all three.
	_ = RunJob(ctx, cfg, job, nil, provider, st, true)
	assertConformanceLineCount(t, taskCounter, 6)
}

func assertConformanceLineCount(t *testing.T, path string, want int) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped counter file this test wrote itself
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	got := 0

	for _, b := range data {
		if b == '\n' {
			got++
		}
	}

	if got != want {
		t.Fatalf("%s: got %d lines, want %d (contents: %q)", path, got, want, data)
	}
}

// TestConformanceGetParamsReachIn verifies that a get step's params: are
// rendered into the resource type's in: command, matching Concourse, where a
// get's params tell the resource HOW to fetch (as opposed to source:, which
// says what to fetch).
//
// Concourse doc: concourse-ci.org/docs/steps/get/ ("params: A map of
// arbitrary configuration to forward to the resource's in script"). Written
// spec page, not a source reading.
//
// steps claim under test: internal/config/step.go's Step.Params field doc,
// internal/resource/resource.go's RunIn doc, docs/resources.md's `in` section.
//
// The second half is the part that is easy to get wrong and expensive when
// wrong: params change what lands in the artifact, so two gets of the SAME
// version differing only in params must not share a cache entry. A depth: 1
// clone reused for a full-history get is a wrong answer that looks like a hit.
func TestConformanceGetParamsReachIn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	// in: writes whatever depth it was handed. The task then proves the value
	// arrived, rather than asserting on the rendered command string.
	pipelineYAML := `
resource_types:
- name: dummy
  config:
    check: printf '[{"ref":"v1"}]'
    in: printf '%s' {{ index .params "depth" | default "default" | shellquote }} > ./depth

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: shallow
  plan:
  - get: thing
    params: { depth: "1" }
  - task: check-shallow
    inputs: [thing]
    run: test "$(cat thing/depth)" = 1

- name: deep
  plan:
  - get: thing
    params: { depth: "full" }
  - task: check-deep
    inputs: [thing]
    run: test "$(cat thing/depth)" = full

- name: unset
  plan:
  - get: thing
  - task: check-unset
    inputs: [thing]
    run: test "$(cat thing/depth)" = default
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	// Each job asserts its own params arrived. "unset" is the third case and
	// the one worth pinning: templates render with missingkey=error, so a
	// resource type offering an OPTIONAL param spells it the way an optional
	// source: field is already spelled (index ... | default, see
	// docs/resources.md). A get with no params: block must reach that default
	// rather than failing the fetch.
	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		runErr := RunJob(ctx, cfg, job, nil, provider, st, false)
		if runErr != nil {
			t.Errorf("job %q: RunJob returned %v, want nil", job.Name, runErr)
		}
	}
}

// TestConformanceAbortFiresOnAbortHook covers the last of the five hook
// modifiers to have no deterministic trigger.
//
// on_error got one as a YAML fixture (docs/attempts-timeout.md's doc-tested
// deadline job, where a per-attempt timeout: classifies as Errored).
// on_abort cannot be one: it requires a CANCELLED JOB CONTEXT — SIGINT/SIGTERM
// mid-run — which a pipeline file has no way to ask for. So it lives here,
// where the context is ours to cancel.
//
// Concourse doc: concourse-ci.org/docs/steps/ (the on_abort hook, "if the step
// is aborted"). steps claim under test: internal/pipeline/hooks.go's
// five-modifier claim, and internal/outcome's Classify, which returns Aborted
// whenever ctx.Err() != nil regardless of the underlying error.
//
// The load-bearing assertion is that on_abort fires and on_failure does NOT:
// a cancelled build reports a killed process as an *exec.ExitError, so a
// classifier that only looked at the error — not at the context — would call
// it a task failure and run the wrong hook.
func TestConformanceAbortFiresOnAbortHook(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "hooks-fired.txt")
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: slow
    run: sleep 30
    on_abort:
      task: note-abort
      run: echo aborted >> %s
    on_failure:
      task: note-failure
      run: echo failed >> %s
`, marker, marker)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Cancelled while the step is in flight, which is what SIGINT does. The
	// hook itself still runs: hooks reached after cancellation get a grace
	// period on a context detached from the cancelled one, or an abort could
	// never have a reaction at all.
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	runErr := RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false)
	if runErr == nil {
		t.Fatal("RunJob returned nil; want a non-nil error for an aborted run")
	}

	if got := outcome.Classify(ctx, runErr); got != outcome.Aborted {
		t.Errorf("outcome.Classify(runErr) = %q, want %q", got, outcome.Aborted)
	}

	fired, err := os.ReadFile(marker) //nolint:gosec // a t.TempDir()-scoped marker this test wrote itself
	if err != nil {
		t.Fatalf("no hook wrote the marker file, so neither hook fired: %v", err)
	}

	got := strings.TrimSpace(string(fired))
	if got != "aborted" {
		t.Errorf("hook marker = %q, want %q — an aborted step must fire on_abort and not on_failure", got, "aborted")
	}
}

// TestConformancePutProducesNoArtifact pins a DIVERGENCE from Concourse,
// deliberately: a put runs out: and nothing else. Concourse follows a
// successful put with an implicit get of the produced version
// (concourse-ci.org/docs/steps/put/); steps removed that in the DSL audit —
// an artifact appearing in the build that no step declared is ambient data
// flow, and every flow here is opt-in. The spelling is an explicit get:
// after the put. See docs/conformance.md and docs/resources.md.
//
// Two runnable jobs plus a load-time assertion. The load-time half is the one
// that actually pins the divergence: `inputs: [thing]` after a bare put must
// be REFUSED, because the put produced no artifact. (Asserting `test ! -d
// thing` inside a task would prove nothing — under per-step isolation an
// undeclared artifact never appears in a step's directory whether the put
// produced one or not.) The runnable jobs cover the opt-in spelling and the
// pre-existing silent-out: divergence, where RunOut allows an out: that
// prints no version, relied on by resource types that publish without
// versioning.
// assertConsumingABarePutIsRefused is the load-bearing half of
// TestConformancePutProducesNoArtifact: a put publishes and produces nothing,
// so a later step naming it as an input must be refused at plan time. This is
// what would break if the implicit get came back. Split out to keep the test
// itself inside the linter's complexity budget.
func assertConsumingABarePutIsRefused(t *testing.T, dir, resourcesYAML string) {
	t.Helper()

	badPath := filepath.Join(dir, "consumes-a-put.yml")

	err := os.WriteFile(badPath, []byte(resourcesYAML+`
jobs:
- name: consume-what-the-put-did-not-produce
  plan:
  - put: thing
  - task: read-it
    inputs: [thing]
    run: "true"
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	badCfg, err := config.LoadConfig(badPath)
	if err != nil {
		t.Fatal(err)
	}

	// The flow check runs per job (RunJob calls it before any step executes,
	// and `steps validate` calls it for every job), not at LoadConfig.
	err = workspace.ValidateArtifactFlow(badCfg, &badCfg.Jobs[0])
	if err == nil {
		t.Fatal("a task consuming a bare put's resource validated; a put produces no artifact, so this must be a plan-time error")
	}

	if !strings.Contains(err.Error(), `input "thing" is not a resource fetched`) {
		t.Errorf("error = %v, want it to name the unavailable input", err)
	}
}

func TestConformancePutProducesNoArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	const resourcesYAML = `
resource_types:
- name: dummy
  config:
    check: printf '[{"ref":"v1"}]'
    in: printf '%s' {{ .version.ref | shellquote }} > ./ref
    out: printf '{"ref":"published-1"}'
- name: silent
  config:
    check: printf '[{"ref":"v1"}]'
    in: printf 'fetched' > ./marker
    out: "true"

resources:
- name: thing
  type: dummy
  source: {}
- name: quiet
  type: silent
  source: {}
`

	assertConsumingABarePutIsRefused(t, dir, resourcesYAML)

	pipelineYAML := resourcesYAML + `
jobs:
# The opt-in spelling: an explicit get after the put. It fetches the
# resource's latest version like any other get (here what check: reports).
- name: explicit-get-after-put
  plan:
  - put: thing
  - get: thing
  - task: read-it
    inputs: [thing]
    run: test "$(cat thing/ref)" = v1

# A put whose out: printed no version still succeeds.
- name: silent-out-still-succeeds
  plan:
  - put: quiet
  - task: nothing-to-do
    run: "true"
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		runErr := RunJob(ctx, cfg, job, nil, provider, st, false)
		if runErr != nil {
			t.Errorf("job %q: RunJob returned %v, want nil", job.Name, runErr)
		}
	}
}

// TestConformanceGetVersionEveryTakesEachVersionOnce pins the cursor that
// version: every needs and, until now, did not have.
//
// Concourse's fan-out walks versions the job has not built
// (atc/db/versions_db.go's NextEveryVersion, github.com/concourse/concourse
// @ v8.2.4 — read from source, not a spec page); steps re-ran everything the
// check still returned. Harmless while the plan is cacheable, and NOT harmless
// with a put: or an agent: in it, which route.go's unskippableReason never
// skips: found in the wild as a Slack bot re-answering every mention still in
// its check's window, once per new mention.
//
// The put: below is the point. Replace it with a task and the merkle cache
// alone would pass this test.
func TestConformanceGetVersionEveryTakesEachVersionOnce(t *testing.T) {
	dir := t.TempDir()
	posted := filepath.Join(dir, "posted.txt")
	versionsFile := filepath.Join(dir, "versions.json")

	cfg, st := everyVersionFixture(t, dir, posted, versionsFile)
	ctx := context.Background()
	job := &cfg.Jobs[0]

	writeEveryVersions(t, versionsFile, `[{"ref":"v1"}]`)
	mustRunEvery(ctx, t, cfg, job, st, false)
	assertConformanceLineCount(t, posted, 1)

	// A second version appears, exactly as a second Slack mention does. The
	// check still returns the first one, because a check reports what exists.
	writeEveryVersions(t, versionsFile, `[{"ref":"v1"},{"ref":"v2"}]`)
	mustRunEvery(ctx, t, cfg, job, st, false)

	// Two posts total, not three: v1 was already taken. Before the cursor
	// this was 3, and grew by one every time anything new arrived.
	assertConformanceLineCount(t, posted, 2)

	// A third run with nothing new posts nothing at all, and still succeeds:
	// "no new versions" is idle, not broken.
	mustRunEvery(ctx, t, cfg, job, st, false)
	assertConformanceLineCount(t, posted, 2)

	// `steps plan` reads the same cursor, so it does not advertise work a run
	// would not do — the planner and the executor share one filtered view by
	// construction (see resource.WithConsumed).
	rows, err := Explain(ctx, cfg, job, nil, st)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("Explain lists %d step(s) with every version already taken: %+v", len(rows), rows)
	}

	// --force is the documented way back: it ignores the cursor along with
	// every other piece of persisted state, so both versions run again.
	mustRunEvery(ctx, t, cfg, job, st, true)
	assertConformanceLineCount(t, posted, 4)

	// ...and having re-run them, it has TAKEN them. A forced run performs the
	// effects like any other, so the ordinary run after it must post nothing.
	mustRunEvery(ctx, t, cfg, job, st, false)
	assertConformanceLineCount(t, posted, 4)
}

// TestGetVersionEveryForceRecordsWhatItTook pins the half of --force the test
// above cannot see: a version FIRST encountered by a forced run.
//
// --force switches the cursor's suppression off, not its recording. When it
// skipped recording too, a forced run performed every effect and remembered
// none, so the next ordinary run performed them all again — the Slack bot
// answering twice, reintroduced by the flag documented as the way to recover
// from it. The versions already recorded before the force hid this, which is
// why it needs a version the force is the first to see.
func TestGetVersionEveryForceRecordsWhatItTook(t *testing.T) {
	dir := t.TempDir()
	posted := filepath.Join(dir, "posted.txt")
	versionsFile := filepath.Join(dir, "versions.json")

	cfg, st := everyVersionFixture(t, dir, posted, versionsFile)
	ctx := context.Background()
	job := &cfg.Jobs[0]

	writeEveryVersions(t, versionsFile, `[{"ref":"v1"}]`)
	mustRunEvery(ctx, t, cfg, job, st, false)
	assertConformanceLineCount(t, posted, 1)

	// v2 arrives and the operator forces: v1 re-posts (the accepted cost of
	// --force) and v2 posts for the first time.
	writeEveryVersions(t, versionsFile, `[{"ref":"v1"},{"ref":"v2"}]`)
	mustRunEvery(ctx, t, cfg, job, st, true)
	assertConformanceLineCount(t, posted, 3)

	// v2 was taken by the forced run, so nothing is left to do.
	mustRunEvery(ctx, t, cfg, job, st, false)
	assertConformanceLineCount(t, posted, 3)
}

// everyVersionFixture builds the pipeline the test above runs: a get with
// version: every feeding a put, whose out: appends to a file. The put is the
// point — replace it with a task and the merkle cache alone would pass.
func everyVersionFixture(t *testing.T, dir, posted, versionsFile string) (*config.Config, *store.Store) {
	t.Helper()

	path := filepath.Join(dir, "pipeline.yml")
	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: cat %s
    in: echo {{ .version.ref | shellquote }} > ./ref
    out: cat thing/ref >> %s

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
    version: every
  - put: thing
    inputs: [thing]
`, versionsFile, posted)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	writeEveryVersions(t, versionsFile, `[]`)

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return cfg, st
}

func writeEveryVersions(t *testing.T, path, versions string) {
	t.Helper()

	err := os.WriteFile(path, []byte(versions), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func mustRunEvery(ctx context.Context, t *testing.T, cfg *config.Config, job *config.Job, st *store.Store, force bool) {
	t.Helper()

	provider, err := workspace.NewProvider(nil, false)
	if err != nil {
		t.Fatal(err)
	}

	err = RunJob(ctx, cfg, job, nil, provider, st, force)
	if err != nil {
		t.Fatalf("RunJob(force=%v): %v", force, err)
	}
}

// TestConformanceRunReadsButNeverAdvancesCheckCursor covers the run half of
// the check cursor: a check is given the last version this pipeline recorded
// for the resource, so it can ask its API for exactly what it has not seen.
//
// Concourse doc: concourse-ci.org/docs/resource-types/implementing/ ("check"
// section) — check "is given the configured source and current version on
// stdin". Written spec page, not a source reading.
//
// steps claim under test: internal/resource/resource.go's CheckVersions doc,
// internal/resource/cache.go's WithLastChecked doc, docs/resources.md's
// `check` section.
//
// The second assertion is the one worth having. steps records that version
// per RESOURCE, and steps watch compares it to decide whether anything is new
// — so if a plain `steps run` advanced it, the watcher's baseline would move
// past a version no watch loop ever enqueued and the trigger for it would
// never fire. Reading and writing are split deliberately: this path reads.
func TestConformanceRunReadsButNeverAdvancesCheckCursor(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.txt")
	path := filepath.Join(dir, "pipeline.yml")

	// The check reports the cursor it was handed, both into the artifact (so
	// the task can prove what the get resolved) and into a log (so a check
	// that ran with no cursor is visible even when nothing downstream reads
	// it). The default is what a first-ever check must fall back to:
	// templates render with missingkey=error, so a bare {{ .version.ref }}
	// would be a hard failure on the first poll of every pipeline.
	pipelineYAML := fmt.Sprintf(`
resource_types:
- name: dummy
  config:
    check: |
      cursor='{{ index .version "ref" | default "cold" }}'
      echo "$cursor" >> %s
      printf '[{"ref": "%%s"}]' "$cursor"
    in: echo {{ .version.ref | shellquote }} > ./ref

resources:
- name: thing
  type: dummy
  source: {}

jobs:
- name: build
  plan:
  - get: thing
  - task: work
    inputs: [thing]
    run: test "$(cat thing/ref)" = seeded
`, seen)

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()

	// What a prior `steps watch` poll would have left behind.
	const seeded = `{"ref":"seeded"}`

	err = st.RecordCheckedVersion(ctx, "thing", seeded)
	if err != nil {
		t.Fatal(err)
	}

	mustRunEvery(ctx, t, cfg, &cfg.Jobs[0], st, false)

	// The task above already failed the run if the artifact said otherwise;
	// this pins the check's own view, which is what the contract is about.
	assertEveryLineIs(t, seen, "seeded")

	// And the record is exactly as the watcher left it.
	after, found, err := st.LastCheckedVersion(ctx, "thing")
	if err != nil {
		t.Fatal(err)
	}

	if !found || after != seeded {
		t.Errorf("recorded version after a run = %q (found=%v), want %q unchanged — only steps watch advances it",
			after, found, seeded)
	}
}

// assertEveryLineIs fails unless path holds at least one line and every line
// equals want. The at-least-one clause matters: an empty file would otherwise
// pass a loop over its lines, and "the check never ran" is the failure most
// worth catching.
func assertEveryLineIs(t *testing.T, path, want string) {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // path is a t.TempDir()-scoped file the test wrote itself
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	lines := strings.Fields(string(data))
	if len(lines) == 0 {
		t.Fatalf("%s is empty", path)
	}

	for _, line := range lines {
		if line != want {
			t.Errorf("%s: got %q, want every line to be %q", path, line, want)
		}
	}
}
