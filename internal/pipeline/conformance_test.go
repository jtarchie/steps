package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
