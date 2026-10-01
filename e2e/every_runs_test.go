package e2e

// `version: every` builds one RUN per version, as Concourse builds one build
// per version: max_in_flight, serial:, the job's history and a retry all count
// builds, and a run that held several made each of them wrong.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
)

const everyRunsPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      cursor='{{ index .version "n" | default "0" }}'
      awk -v c="$cursor" 'BEGIN{printf "["} $1+0 > c+0 {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' {{ .source.file }}
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: a
  type: feed
  source: {file: FEED_A}
- name: b
  type: feed
  source: {file: FEED_B}
jobs:
- name: build
  plan:
  - get: a
    trigger: true
    version: every
  - get: b
    trigger: true
  - task: work
    inputs: [a, b]
    run: echo "$(cat a/n.txt)+$(cat b/n.txt)" >> PROCESSED
`

// runVersions is, per run of job, newest first, the versions of resource it
// was created with.
func runVersions(t *testing.T, st store.Store, job, resource string) [][]string {
	t.Helper()

	runs, err := st.ListRuns(t.Context(), job, 0)
	if err != nil {
		t.Fatal(err)
	}

	got := make([][]string, 0, len(runs))

	for _, run := range runs {
		inputs, err := st.RunInputs(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}

		var versions []string

		for _, input := range inputs {
			if input.Resource == resource {
				versions = append(versions, input.Version)
			}
		}

		got = append(got, versions)
	}

	return got
}

// TestWatchEveryVersionIsItsOwnRun is the reported bug: three versions found
// by one poll are three runs on the job's page, each built from one version,
// not one run holding three builds.
func TestWatchEveryVersionIsItsOwnRun(t *testing.T) {
	fixture := newLockstepFixture(t, everyRunsPipeline)
	fixture.coldStart(t)

	fixture.feed(t, fixture.feedA, 4)
	fixture.watch(t)
	fixture.assertDid(t, "2+1", "3+1", "4+1")

	st := waitForStore(t, fixture.db, "pipeline")
	defer func() { _ = st.Close() }()

	got := runVersions(t, st, "build", "a")
	want := [][]string{{`{"n":"4"}`}, {`{"n":"3"}`}, {`{"n":"2"}`}, {`{"n":"1"}`}}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("runs of build, newest first, were created with %v; want one version each: %v", got, want)
	}
}

// TestWatchEveryVersionRunsOverlapUnderMaxInFlight: two versions under
// max_in_flight: 2 are two builds at once, which a run holding both could
// never be. Each build waits for the other to start, so a daemon that builds
// them one after another fails the first one's barrier. The cold start's
// build of version 1 has no sibling and skips it.
func TestWatchEveryVersionRunsOverlapUnderMaxInFlight(t *testing.T) {
	started := filepath.Join(t.TempDir(), "started")

	err := os.MkdirAll(started, 0o750)
	if err != nil {
		t.Fatal(err)
	}

	fixture := newLockstepFixture(t, strings.ReplaceAll(mustReplace(t, mustReplace(t, everyRunsPipeline,
		"- name: build\n  plan:",
		"- name: build\n  max_in_flight: 2\n  plan:"),
		`run: echo "$(cat a/n.txt)+$(cat b/n.txt)" >> PROCESSED`,
		`run: |
      n=$(cat a/n.txt)
      if [ "$n" != 1 ]; then
        touch STARTED/$n
        i=0
        while [ "$(ls STARTED | wc -l)" -lt 2 ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done
        [ "$(ls STARTED | wc -l)" -ge 2 ] || exit 1
      fi
      echo "$n" >> PROCESSED`), "STARTED", started))

	fixture.serve(t, "--max-concurrent", "2")
	fixture.coldStart(t)

	fixture.feed(t, fixture.feedA, 3)
	fixture.watch(t)

	data, err := os.ReadFile(fixture.processed)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.Fields(string(data))
	slices.Sort(got)

	if !slices.Equal(got, []string{"2", "3"}) {
		t.Errorf("max_in_flight: 2 built %v, want versions 2 and 3 once each, side by side", got)
	}
}

// TestRunBuildsTheOldestVersionOnly: a manual `steps run` is one build, as
// `fly trigger-job` is — the oldest version the job has not taken — and the
// next invocation takes the next.
func TestRunBuildsTheOldestVersionOnly(t *testing.T) {
	dir := t.TempDir()
	processed := filepath.Join(dir, "processed.txt")

	path := writePipeline(t, dir, fmt.Sprintf(`
resource_types:
- name: ticker
  config:
    check: printf '[{"n":"1"},{"n":"2"},{"n":"3"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: ticks
  type: ticker
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: work
    inputs: [ticks]
    run: cat ticks/n.txt >> %s
`, processed))

	run := func() {
		t.Helper()

		var err error

		out := captureStdout(t, func() { err = cli.Run([]string{"run", path, "--job", "build"}) })
		if err != nil {
			t.Fatalf("run: %v", err)
		}

		if !strings.Contains(out, backlogNote) {
			t.Errorf("a run that left versions waiting did not say so:\n%s", out)
		}
	}

	assertProcessed := func(want ...string) {
		t.Helper()

		data, err := os.ReadFile(processed) //nolint:gosec // a t.TempDir()-scoped file this test wrote itself
		if err != nil {
			t.Fatal(err)
		}

		if got := strings.Fields(string(data)); !slices.Equal(got, want) {
			t.Errorf("built %v, want %v", got, want)
		}
	}

	run()
	assertProcessed("1")

	run()
	assertProcessed("1", "2")

	st := openStoreFor(t, path)

	if got := runVersions(t, st, "build", "ticks"); fmt.Sprint(got) != fmt.Sprint([][]string{{`{"n":"2"}`}, {`{"n":"1"}`}}) {
		t.Errorf("runs were created with %v, want one version each", got)
	}
}

// TestStepsTestBuildsEveryVersionAsItsOwnRun: `steps test` re-opens taken
// versions, so it cannot walk a backlog one claim at a time — it builds every
// version, each as its own run, and a job's assert: holds over all of them.
func TestStepsTestBuildsEveryVersionAsItsOwnRun(t *testing.T) {
	dir := t.TempDir()

	path := writePipeline(t, dir, `
resource_types:
- name: ticker
  config:
    check: printf '[{"n":"1"},{"n":"2"}]'
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: ticks
  type: ticker
  source: {}
jobs:
- name: build
  plan:
  - get: ticks
    version: every
  - task: work
    run: 'true'
  assert:
    execution: [ticks, work, ticks, work]
`)

	err := cli.Run([]string{"test", path})
	if err != nil {
		t.Fatalf("steps test: %v", err)
	}

	st := openStoreFor(t, path)

	if got := runVersions(t, st, "build", "ticks"); fmt.Sprint(got) != fmt.Sprint([][]string{{`{"n":"2"}`}, {`{"n":"1"}`}}) {
		t.Errorf("steps test created runs with %v, want one per version", got)
	}

	// Judged per run, each would fail an assert written for the whole job.
	runs, err := st.ListRuns(t.Context(), "build", 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, run := range runs {
		if run.Status != "succeeded" {
			t.Errorf("run %s is %s: a run judged the job's assert: over its own version alone", run.ID, run.Status)
		}
	}
}

// backlogNote is what `steps run` says when versions still wait behind the
// one it built.
const backlogNote = "waiting; run it again to build the next"

// runBacklog runs args until no version is left waiting — the local
// equivalent of the daemon walking a backlog — joining every run's error.
func runBacklog(t *testing.T, args ...string) error {
	t.Helper()

	var errs []error

	for range 50 {
		var err error

		out := captureStdout(t, func() { err = cli.Run(args) })
		if err != nil {
			errs = append(errs, err)
		}

		if !strings.Contains(out, backlogNote) {
			return errors.Join(errs...)
		}
	}

	t.Fatalf("%v never ran out of waiting versions", args)

	return nil
}

func mustRunBacklog(t *testing.T, args ...string) {
	t.Helper()

	err := runBacklog(t, args...)
	if err != nil {
		t.Fatalf("run(%v): %v", args, err)
	}
}
