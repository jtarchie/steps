package e2e

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestResumeFollowsTheRouteTheFirstAttemptTook is steps#231: a resume walked the plan in written order, so a step the first attempt's to: jumped past was run (an escalation, a second deploy), and a to: failure route's step was re-run because its consumed failure was never recorded as done.
func TestResumeFollowsTheRouteTheFirstAttemptTook(t *testing.T) {
	for name, route := range map[string]struct {
		cmd, key string
	}{
		"success": {"true", "success"},
		"failure": {"false", "failure"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			log := func(step string) string { return filepath.Join(dir, step+".log") }
			flag := filepath.Join(dir, "fixed")

			path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: first
    run: echo ran >> %[1]s; %[2]s
    to: { %[3]s: target }
  - task: middle
    run: echo ran >> %[4]s
  - task: target
    run: echo ran >> %[5]s
  - task: fragile
    run: echo ran >> %[6]s; test -f %[7]s
`, log("first"), route.cmd, route.key, log("middle"), log("target"), log("fragile"), flag))

			runID, _ := failThenCapture(t, path)
			assertLineCount(t, log("first"), 1)
			assertNoFile(t, log("middle"))
			assertLineCount(t, log("target"), 1)

			writePipelineFile(t, flag, "")

			out := captureStdout(t, func() {
				err := cli.Run([]string{"run", path, "--resume", runID})
				if err != nil {
					t.Fatalf("resume failed: %v", err)
				}
			})

			assertLineCount(t, log("first"), 1)
			assertNoFile(t, log("middle"))
			assertLineCount(t, log("target"), 1)
			assertLineCount(t, log("fragile"), 2)

			want := fmt.Sprintf("route: first --%s--> target (visit 1, replayed)\n", route.key)
			if !strings.Contains(out, want) {
				t.Errorf("the replayed route was not reported as %q:\n%s", want, out)
			}
		})
	}
}

// TestResumeKeepsTheLoopsVisitCount: a resumed run started max_visits at zero, so a loop interrupted near its limit got a fresh budget.
func TestResumeKeepsTheLoopsVisitCount(t *testing.T) {
	dir := t.TempDir()
	bumps := filepath.Join(dir, "bumps.log")
	flag := filepath.Join(dir, "fixed")

	// fragile fails on the second visit until fixed, so the first attempt stops after check has routed once.
	path := writePipeline(t, dir, fmt.Sprintf(`
jobs:
- name: build
  plan:
  - task: bump
    run: echo visit >> %[1]s
  - task: fragile
    run: test "$(wc -l < %[1]s)" -ne 2 || test -f %[2]s
  - task: check
    run: "false"
    to: { failure: bump }
    max_visits: 3
`, bumps, flag))

	runID, _ := failThenCapture(t, path)
	assertLineCount(t, bumps, 2)

	writePipelineFile(t, flag, "")

	var err error

	out := captureStdout(t, func() {
		err = cli.Run([]string{"run", path, "--resume", runID})
	})

	if err == nil || !strings.Contains(err.Error(), "exceeded max_visits 3") {
		t.Fatalf("the resumed loop should have exhausted its original budget, got err=%v:\n%s", err, out)
	}

	assertLineCount(t, bumps, 3)
}
