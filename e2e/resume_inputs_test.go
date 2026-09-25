package e2e

// A resume rebuilds each build against every version it was created with —
// fly rerun-build's rule, from build_resource_config_version_inputs: every
// input of the build, fixed gets and pins included, never what is newest.

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// recordedInputsPipeline fans out over ticks and binds config beside it. The
// put publishes both versions, so a build re-bound to the wrong config is a
// line that says so; a put is never cache-skipped, so a build wrongly passed
// over publishes nothing.
func recordedInputsPipeline(ticks, config, published, flag string) string {
	return fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat {{ .source.file | shellquote }}
    in: echo {{ .version.n | shellquote }} > n.txt
- name: recorder
  config:
    out: |
      echo "$(cat config/n.txt) $(cat ticks/n.txt)" >> %[3]s
      printf '{"published":"yes"}\n'

resources:
- name: ticks
  type: counter
  source: {file: %[1]s}
- name: config
  type: counter
  source: {file: %[2]s}
- name: publication
  type: recorder
  source: {}

jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - get: config
  - task: fragile
    inputs: [ticks]
    run: test "$(cat ticks/n.txt)" != two || test -f %[4]s
  - put: publication
    inputs: [ticks, config]
`, ticks, config, published, flag)
}

// TestResumeBindsAFixedGetToTheVersionItsBuildWasCreatedWith: only the
// fan-out's versions were recorded per build, so a resume re-resolved every
// other get to whatever the check reports now — and build #1's completed
// steps, recorded against config@c1, would have been trusted inside a build
// binding config@c2. Concourse records every input of a build and reruns
// against exactly those.
func TestResumeBindsAFixedGetToTheVersionItsBuildWasCreatedWith(t *testing.T) {
	dir := t.TempDir()

	ticks := filepath.Join(dir, "ticks.json")
	config := filepath.Join(dir, "config.json")
	published := filepath.Join(dir, "published.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, ticks, `[{"n":"one"},{"n":"two"}]`)
	writePipelineFile(t, config, `[{"n":"c1"}]`)

	path := writePipeline(t, dir, recordedInputsPipeline(ticks, config, published, flag))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected build #1 to fail")
		}
	})

	runID := resumeID(t, out)
	assertLineCount(t, published, 1)

	// config moves between the attempts, as a re-pushed branch would.
	writePipelineFile(t, config, `[{"n":"c1"},{"n":"c2"}]`)
	writePipelineFile(t, flag, "")

	err := cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	got := readFileString(t, published)
	if got != "c1 one\nc1 two\n" {
		t.Errorf("published:\n%s\nwant build #1 rebuilt with the config it was created with", got)
	}
}

// TestResumeRebuildsTheVersionAFixedGetWasCreatedWith is the same rule with
// no fan-out at all, which is nearly every job: a plain get: whose version
// moved while the run sat failed is rebuilt at the version the run began
// with, not the new one — that is what makes "continue THIS run" mean
// anything.
func TestResumeRebuildsTheVersionAFixedGetWasCreatedWith(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, versions, `[{"n":"one"}]`)

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: counter
  config:
    check: cat %[1]s
    in: echo {{ .version.n | shellquote }} > n.txt

resources:
- name: repo
  type: counter
  source: {}

jobs:
- name: build
  plan:
  - get: repo
  - task: fragile
    inputs: [repo]
    run: |
      cat repo/n.txt >> %[2]s
      test -f %[3]s
`, versions, ran, flag))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build"})
		if err == nil {
			t.Fatal("expected the fragile step to fail")
		}
	})

	runID := resumeID(t, out)

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"}]`)
	writePipelineFile(t, flag, "")

	err := cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if got := readFileString(t, ran); got != "one\none\n" {
		t.Errorf("ran.log:\n%s\nwant the resume to rebuild the version the run was created with", got)
	}
}

// TestResumeOfAPinnedRunKeepsItsPin: a pin is an input like any other in
// Concourse's table, so a rerun of a pinned build binds the pin. A resume
// that dropped it fanned out over the whole unconsumed history instead —
// and, the pinned run having taken nothing, the cursor still owes every
// other version to the next ordinary run.
func TestResumeOfAPinnedRunKeepsItsPin(t *testing.T) {
	dir := t.TempDir()

	versions := filepath.Join(dir, "versions.json")
	ran := filepath.Join(dir, "ran.log")
	published := filepath.Join(dir, "published.log")
	flag := filepath.Join(dir, "fixed")

	writePipelineFile(t, versions, `[{"n":"one"},{"n":"two"},{"n":"three"}]`)

	path := writePipeline(t, dir, fanOutPipeline(versions, ran, published, flag, ""))

	out := captureStdout(t, func() {
		err := cli.Run([]string{"run", path, "--job", "build", "--pin", "n=two"})
		if err == nil {
			t.Fatal("expected the pinned build to fail")
		}
	})

	runID := resumeID(t, out)
	assertLineCount(t, ran, 1)

	writePipelineFile(t, flag, "")

	err := cli.Run([]string{"run", path, "--resume", runID})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}

	if got := readFileString(t, ran); got != "two\ntwo\n" {
		t.Errorf("ran.log:\n%s\nwant the resume to build the pinned version and nothing else", got)
	}

	// The cursor never took the pin, so an ordinary run still owes every version.
	err = cli.Run([]string{"run", path, "--job", "build"})
	if err != nil {
		t.Fatalf("the ordinary run after the pinned one failed: %v", err)
	}

	if got := readFileString(t, ran); !strings.HasSuffix(got, "one\ntwo\nthree\n") {
		t.Errorf("ran.log:\n%s\nwant the ordinary run to fan out over the versions the pin left untaken", got)
	}
}
