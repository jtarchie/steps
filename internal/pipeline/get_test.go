package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// TestAResumedPinnedRunLeavesTheCheckedVersionAlone: the pin exemption holds on a resume too — the get kept from a pinned attempt records nothing, or an older pinned version would drag the displayed checked version back.
func TestAResumedPinnedRunLeavesTheCheckedVersionAlone(t *testing.T) {
	dir := t.TempDir()
	feed := filepath.Join(dir, "feed.json")
	fixed := filepath.Join(dir, "fixed")

	writeFixture(t, feed, `[{"n":"v1"},{"n":"v2"}]`)
	writeFixture(t, fixed, "")

	cfg, job, st, provider := fixtureFrom(t, fmt.Sprintf(`
resource_types:
- name: listing
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: ticks
  type: listing
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
  - task: expensive
    inputs: []
    run: "true"
  - task: fragile
    inputs: []
    run: test -f %[2]s
`, feed, fixed))

	defer func() { _ = st.Close() }()
	defer func() { _ = provider.Close() }()

	err := RunJob(context.Background(), cfg, job, st, Run{ID: NewRunID(), Provider: provider})
	if err != nil {
		t.Fatalf("unpinned run: %v", err)
	}

	err = os.Remove(fixed)
	if err != nil {
		t.Fatal(err)
	}

	pin := map[string]string{"n": "v1"}
	runID := NewRunID()

	err = RunJob(context.Background(), cfg, job, st, Run{ID: runID, Provider: provider, Pinned: pin})
	if err == nil {
		t.Fatal("the pinned run passed before it was fixed")
	}

	writeFixture(t, fixed, "")

	ctx, workspaceDir, err := PrepareResume(context.Background(), st, runID)
	if err != nil {
		t.Fatalf("PrepareResume: %v", err)
	}

	provider.Reuse(workspaceDir)

	out := captureStdout(t, func() { err = RunJob(ctx, cfg, job, st, Run{Provider: provider, Pinned: pin}) })
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !strings.Contains(out, "skip: ticks (already fetched)") {
		t.Fatalf("the resume did not keep the get, so this test proves nothing:\n%s", out)
	}

	baseline, _, err := lastCheckedVersion(t, st, "ticks")
	if err != nil {
		t.Fatal(err)
	}

	if baseline != `{"n":"v2"}` {
		t.Errorf("baseline = %s, want unchanged v2 — a resumed pinned run's kept get must not regress the checked version", baseline)
	}
}

type resumeRuns struct {
	store.Store

	err error
}

func (r resumeRuns) ResumeRun(context.Context, string, string, string) error { return r.err }

// TestAnUnrecordedBuildTreeIsLogged: re-pointing the run at its build's tree is bookkeeping a get does not fail over, so a failure is said once, naming the run whose resume would then look in the wrong place.
func TestAnUnrecordedBuildTreeIsLogged(t *testing.T) {
	logged := captureSlog(t)

	w := &planWalk{stepRunner: stepRunner{cfg: &config.Config{}, st: resumeRuns{err: os.ErrPermission}}}
	w.pointRunAt(withResume(context.Background(), newRunState("r-9")), "/tmp/build")

	if line := logged.String(); !strings.Contains(line, "msg=job.run_workspace_unrecorded") || !strings.Contains(line, "run=r-9") {
		t.Errorf("logged %q, want job.run_workspace_unrecorded naming the run", line)
	}

	logged.Reset()

	w.st = resumeRuns{}
	w.pointRunAt(withResume(context.Background(), newRunState("r-9")), "/tmp/build")

	if logged.Len() != 0 {
		t.Errorf("a recorded tree logged %q", logged.String())
	}
}
