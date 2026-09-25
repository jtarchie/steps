package pipeline

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// TestAResumedPutStillReadsItsVersions: a resume re-fetches rather than
// skipping gets, so the versions a put reads are there again without any
// resume-specific bookkeeping — including an in-place get's.
func TestAResumedPutStillReadsItsVersions(t *testing.T) {
	dir := t.TempDir()
	fixed := filepath.Join(dir, "fixed")

	cfg, job, st, provider := fixtureFrom(t, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
    in: "true"
- name: reader
  config:
    expr:
      out: 'version("more").n == "1" && version("ticks").n == "1" ? {read: "yes"} : fail("versions missing")'
resources:
- name: ticks
  type: counter
  source: {}
- name: more
  type: counter
  source: {}
- name: read
  type: reader
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - get: more
  - task: fragile
    run: test -f %[1]s
  - put: read
    inputs: [ticks, more]
`, fixed))

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	runID := NewRunID()

	err := RunJob(WithNewRun(context.Background(), runID), cfg, job, nil, provider, st, false)
	if err == nil {
		t.Fatal("the fragile step passed before it was fixed")
	}

	writeFixture(t, fixed, "")

	ctx, workspaceDir, err := PrepareResume(context.Background(), st, runID)
	if err != nil {
		t.Fatalf("PrepareResume: %v", err)
	}

	resumable, ok := provider.(workspace.Resumable)
	if !ok {
		t.Fatal("the default workspace provider cannot resume")
	}

	resumable.Reuse(workspaceDir)

	_ = captureStdout(t, func() { err = RunJob(ctx, cfg, job, nil, provider, st, false) })
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
}

// TestAGetsOwnHookReadsItsVersion: a get's on_success runs before the plan
// moves on, so the version has to be recorded before the hooks, not after —
// on both the triggered first get and an in-place later one.
func TestAGetsOwnHookReadsItsVersion(t *testing.T) {
	cfg, job, st, provider := fixtureFrom(t, `
resource_types:
- name: counter
  config:
    check: printf '[{"n":"1"}]'
    in: "true"
- name: reader
  config:
    expr:
      out: 'version().n == "1" ? {read: "yes"} : fail("no version")'
resources:
- name: ticks
  type: counter
  source: {}
- name: more
  type: counter
  source: {}
- name: read
  type: reader
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
    on_success:
      put: read
      inputs: [ticks]
  - get: more
    on_success:
      put: read
      inputs: [more]
`)

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	var err error

	_ = captureStdout(t, func() {
		err = RunJob(WithNewRun(context.Background(), NewRunID()), cfg, job, nil, provider, st, false)
	})
	if err != nil {
		t.Fatalf("a get's on_success could not read what it fetched: %v", err)
	}
}

// TestPutInputsSnapshot: what a put may read is its declared inputs, fixed at
// the moment it starts — in_parallel siblings keep recording after it.
func TestPutInputsSnapshot(t *testing.T) {
	t.Parallel()

	ctx, _ := withBuildVersions(context.Background())

	big := map[string]any{"blob": strings.Repeat("x", 5<<10)}

	recordFetched(ctx, "b", map[string]any{"id": "2"})
	recordFetched(ctx, "a", map[string]any{})
	recordFetched(ctx, "c", big)

	all := putInputs(ctx, config.Step{Put: "p", Inputs: &config.InputSpec{All: true}})
	if !slices.Equal(all.Names, []string{"a", "b", "c"}) {
		t.Errorf("inputs: all names = %v, want every fetched get, sorted", all.Names)
	}

	// Neither cap on a STORED version applies: an empty version and one over
	// maxRecordedVersionBytes were both fetched, and version() must say so.
	if _, ok := all.Versions["a"]; !ok {
		t.Error("an empty fetched version is missing")
	}

	if _, ok := all.Versions["c"]; !ok {
		t.Error("an oversized fetched version is missing")
	}

	named := putInputs(ctx, config.Step{Put: "p", Inputs: &config.InputSpec{Names: []string{"notes", "b"}}})
	if !slices.Equal(named.Names, []string{"b", "notes"}) || len(named.Versions) != 1 || named.Versions["b"]["id"] != "2" {
		t.Errorf("named inputs = %+v, want [b notes] with only b's version", named)
	}

	recordFetched(ctx, "d", map[string]any{"id": "4"})

	if _, ok := all.Versions["d"]; ok {
		t.Error("a get recorded after the snapshot leaked into it")
	}

	if none := putInputs(ctx, config.Step{Put: "p"}); len(none.Names) != 0 || len(none.Versions) != 0 {
		t.Errorf("a put with no inputs: = %+v, want nothing", none)
	}
}
