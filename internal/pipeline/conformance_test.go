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

// TestConformancePutProducesNoArtifact pins a DIVERGENCE from Concourse,
// deliberately: a put runs out: and nothing else. Concourse follows a
// successful put with an implicit get of the produced version
// (concourse-ci.org/docs/steps/put/); steps removed that in the DSL audit —
// an artifact appearing in the build that no step declared is ambient data
// flow, and every flow here is opt-in. The spelling is an explicit get:
// after the put. See docs/conformance.md and docs/resources.md.
//
// Three jobs: no artifact exists after a bare put; an explicit get after the
// put is the opt-in fetch; and a put whose out: printed no version still
// succeeds (the pre-existing divergence — RunOut allows a silent out:, relied
// on by resource types that publish without versioning).
func TestConformancePutProducesNoArtifact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
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

jobs:
# A put fetches nothing and leaves no artifact behind.
- name: put-produces-no-artifact
  plan:
  - put: thing
  - task: nothing-to-read
    run: test ! -d thing

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
  - task: nothing-was-fetched
    run: test ! -d quiet
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
