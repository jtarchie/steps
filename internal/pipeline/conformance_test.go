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

	// A second invocation (skipCache=false) must skip the already-succeeded
	// v1/v3 chains and retry only v2 — proving the fan-out fix composes
	// correctly with the merkle skip-cache instead of just asserting it in
	// prose.
	_ = RunJob(ctx, cfg, job, nil, provider, st, false)
	assertConformanceLineCount(t, taskCounter, 4)
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
// on_error got one as a YAML fixture (examples/flow.yml's
// timeout-fires-on-error, where a per-attempt timeout: classifies as Errored).
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

// TestConformancePutRunsImplicitGet verifies that a successful put fetches the
// version it produced, so later steps can use it as an artifact named after
// the put.
//
// Concourse doc: concourse-ci.org/docs/steps/put/ — "When the step succeeds,
// the version by the step will be immediately fetched via an additional
// implicit get step. This is so that later steps in your plan can use the
// artifact that was produced." Written spec page, not a source reading.
//
// steps claim under test: internal/pipeline's fetchPutVersion, config.Step's
// GetParams/NoGet docs, docs/resources.md.
//
// The three jobs cover the behaviour and both of its off-switches, because the
// off-switches are where an implementation drifts: one that fetched
// unconditionally would pass a test for the happy path alone.
func TestConformancePutRunsImplicitGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	// out: prints the version it published; in: writes that version's ref plus
	// whatever get_params it was handed, so a task can prove both arrived.
	pipelineYAML := `
resource_types:
- name: dummy
  config:
    check: printf '[{"ref":"v1"}]'
    in: |
      printf '%s' {{ .version.ref | shellquote }} > ./ref
      printf '%s' {{ index .params "flavor" | default "none" | shellquote }} > ./flavor
    out: printf '{"ref":"published-1"}'

resources:
- name: thing
  type: dummy
  source: {}

jobs:
# The version the PUT produced is what the implicit get fetched — published-1,
# not the v1 that check: reports. That distinction is the whole point: without
# the implicit get there is no artifact at all, and with a naive one there is
# an artifact holding the wrong version.
- name: implicit-get-fetches-produced-version
  plan:
  - put: thing
  - task: read-it
    inputs: [thing]
    run: test "$(cat thing/ref)" = published-1

# get_params reach that fetch's in:.
- name: get-params-reach-the-implicit-get
  plan:
  - put: thing
    get_params: { flavor: strawberry }
  - task: read-it
    inputs: [thing]
    run: test "$(cat thing/flavor)" = strawberry

# no_get: skips it, so nothing is fetched and no artifact exists.
- name: no-get-skips-the-fetch
  plan:
  - put: thing
    no_get: true
  - task: nothing-to-read
    run: test ! -d thing
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

	ctx := context.Background()

	for i := range cfg.Jobs {
		job := &cfg.Jobs[i]

		runErr := RunJob(ctx, cfg, job, nil, provider, st, false)
		if runErr != nil {
			t.Errorf("job %q: RunJob returned %v, want nil", job.Name, runErr)
		}
	}
}

// TestConformancePutWithNoVersionSkipsImplicitGet covers the one place steps
// must diverge from Concourse here, and why.
//
// Concourse expects an out: script to print the version it created. steps
// explicitly allows printing nothing (docs/resources.md: "Printing nothing is
// fine and not an error"; RunOut returns a nil version rather than erroring),
// which predates this feature and is relied on by read-only-ish resource types
// that publish without versioning what they published.
//
// So a put with no version has nothing to fetch. It must SUCCEED with no
// artifact rather than failing — inventing a failure here would break those
// resource types the moment the implicit get shipped.
func TestConformancePutWithNoVersionSkipsImplicitGet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resource_types:
- name: silent
  config:
    check: printf '[{"ref":"v1"}]'
    in: printf 'fetched' > ./marker
    out: "true"

resources:
- name: thing
  type: silent
  source: {}

jobs:
- name: build
  plan:
  - put: thing
  - task: nothing-was-fetched
    run: test ! -d thing
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

	runErr := RunJob(context.Background(), cfg, &cfg.Jobs[0], nil, provider, st, false)
	if runErr != nil {
		t.Errorf("RunJob returned %v, want nil — a put whose out: printed no version must succeed, not fail on a fetch it cannot make", runErr)
	}
}
