package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// getAfterPutType is a resource whose check and put disagree: check reports v1, out prints v2, and both append to a log so a test can tell which ran.
func getAfterPutType(log string) string {
	return fmt.Sprintf(`
resource_types:
- name: release
  config:
    check: |
      echo check >> %[1]s
      printf '[{"ref": "v1"}]'
    in: |
      echo in {{ .version.ref }} >> %[1]s
      echo {{ .version.ref | shellquote }} > ref
    out: |
      echo out >> %[1]s
      printf '{"ref": "v2"}'
resources:
- name: releases
  type: release
  source: {}
`, log)
}

func readLog(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path) //nolint:gosec // a t.TempDir() file this test named
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	return string(data)
}

// TestEndToEndAGetAfterAPutFetchesWhatThePutRecorded: a get placed after a put to the same resource fetches exactly the version that put printed, by running in: with it, and no check runs for it at all — not before the plan, not at the get.
func TestEndToEndAGetAfterAPutFetchesWhatThePutRecorded(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	path := writePipeline(t, dir, getAfterPutType(log)+`
jobs:
- name: publish
  plan:
  - put: releases
  - get: releases
  - task: read
    inputs: [releases]
    run: cat releases/ref
    assert:
      stdout: v2
  assert:
    execution: [releases, releases, read]
    outcome: succeeded
`)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "publish", "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if got := readLog(t, log); got != "out\nin v2\n" {
		t.Errorf("the resource ran %q, want the put's out and then one in of v2, and no check", got)
	}

	if !strings.Contains(out, `get: releases (version: {"ref":"v2"}, recorded by put releases)`) {
		t.Errorf("the get's line does not say the put recorded its version:\n%s", out)
	}
}

// TestEndToEndAGetAfterAPutFetchesFromATypeWithNoCheck: a type that can publish and fetch but has nothing to discover is got after its put, which is the shape the built-in publishers have.
func TestEndToEndAGetAfterAPutFetchesFromATypeWithNoCheck(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
resource_types:
- name: ticket
  config:
    in: echo {{ .version.id | shellquote }} > id
    out: |
      printf '{"id": "CON-7"}'
resources:
- name: ticket
  type: ticket
  source: {}
jobs:
- name: file
  plan:
  - put: ticket
  - get: ticket
  - task: title
    inputs: [ticket]
    run: echo "[$(cat ticket/id)]"
    assert:
      stdout: "[CON-7]"
  assert:
    execution: [ticket, ticket, title]
    outcome: succeeded
`)

	mustRun(t, "run", path, "--job", "file")
}

// TestEndToEndAGetAfterAPutWhosePutWasSkippedIsSkipped: a put its when: skipped recorded nothing, so the get after it has nothing to fetch and is skipped with it, and the plan carries on.
func TestEndToEndAGetAfterAPutWhosePutWasSkippedIsSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	path := writePipeline(t, dir, getAfterPutType(log)+`
jobs:
- name: publish
  plan:
  - put: releases
    when: "false"
  - get: releases
  - task: after
    inputs: []
    run: echo carried on
  assert:
    execution: [after]
    outcome: succeeded
`)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "publish", "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if got := readLog(t, log); got != "" {
		t.Errorf("the resource ran %q, want nothing: the put was skipped, and the get with it", got)
	}

	if !strings.Contains(out, "skip: releases (put releases was skipped, so there is no version to fetch)") {
		t.Errorf("the get's skip does not name the put:\n%s", out)
	}
}

// TestEndToEndAGetAfterAFailedTriedPutFails: try: let the put fail and the plan carry on, but the get after it was asked for a version that does not exist, so it fails naming the put rather than fetching some other version.
func TestEndToEndAGetAfterAFailedTriedPutFails(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	path := writePipeline(t, dir, `
resource_types:
- name: release
  config:
    check: |
      printf '[{"ref": "v1"}]'
    in: echo {{ .version.ref | shellquote }} > ref
    out: exit 1
resources:
- name: releases
  type: release
  source: {}
jobs:
- name: publish
  plan:
  - try:
      put: releases
  - get: releases
`)

	err := stepsRun(t, []string{"run", path, "--job", "publish"})
	if err == nil || !strings.Contains(err.Error(), "put releases (step 0) failed, so there is no version to fetch") {
		t.Fatalf("run: %v, want the get to fail naming the put that failed", err)
	}
}

// TestEndToEndAPutInAHookDoesNotBindAGet: a hook only reacts to the step it hangs on, so its put is not the put a later get fetches after — that get resolves through the check as any other does.
func TestEndToEndAPutInAHookDoesNotBindAGet(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	path := writePipeline(t, dir, getAfterPutType(log)+`
jobs:
- name: publish
  plan:
  - task: work
    inputs: []
    run: echo working
    on_success:
      put: releases
  - get: releases
  - task: read
    inputs: [releases]
    run: cat releases/ref
    assert:
      stdout: v1
  assert:
    execution: [work, releases, releases, read]
    outcome: succeeded
`)

	mustRun(t, "run", path, "--job", "publish")
}

// TestEndToEndAResumedGetAfterAPutBindsTheVersionTheFirstAttemptPut: the put ran in the attempt that failed, so a resume skips it — and the get after it still fetches the version that put recorded, rather than nothing or the check's.
func TestEndToEndAResumedGetAfterAPutBindsTheVersionTheFirstAttemptPut(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	flag := filepath.Join(dir, "fixed")

	path := writePipeline(t, dir, getAfterPutType(log)+fmt.Sprintf(`
jobs:
- name: publish
  plan:
  - put: releases
  - task: gate
    inputs: []
    run: test -f %s
  - get: releases
  - task: read
    inputs: [releases]
    run: echo "[$(cat releases/ref)]"
`, flag))

	var err error

	first := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "publish", "--progress", "plain"})
	})
	if err == nil {
		t.Fatalf("expected the gate to fail the first attempt:\n%s", first)
	}

	writePipelineFile(t, flag, "")

	resumed := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--resume", resumeID(t, first), "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("resume: %v\n%s", err, resumed)
	}

	if !strings.Contains(resumed, "[v2]") {
		t.Errorf("the resumed get did not fetch the version the first attempt's put recorded:\n%s", resumed)
	}

	if got := readLog(t, log); got != "out\nin v2\n" {
		t.Errorf("the resource ran %q, want one out (the resume skips the put) and one in of v2", got)
	}
}

// TestEndToEndAGetAfterAGitHubPRPutReadsThePullRequest: the built-in github-pr has an in: for exactly this — the get after the put writes the pull request the put opened, and the step after reads its number, url and title from files. This is the get the bumps pipeline needs between its ticket put and its PR title.
func TestEndToEndAGetAfterAGitHubPRPutReadsThePullRequest(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.pushBranch("feature/widgets", prHeadSHA)

	path := writePipeline(t, t.TempDir(), prPublishPipeline(fake, "Add widgets", "Widgets, plainly.", prTitleBody)+`
  - get: pr
  - task: read
    inputs: [pr]
    run: echo "[$(cat pr/pr.number)|$(cat pr/pr.sha)|$(cat pr/pr.title)|$(cat pr/pr.url)]"
`)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "publish", "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	opened := fake.openedPRs()
	if len(opened) != 1 {
		t.Fatalf("opened %+v, want one pull request", opened)
	}

	want := fmt.Sprintf("[%d|%s|Add widgets|%s]", opened[0].Number, prHeadSHA, fake.html("pull", opened[0].Number))
	if !strings.Contains(out, want) {
		t.Errorf("the step after the get did not read the pull request the put opened, want %s:\n%s", want, out)
	}
}

// TestEndToEndAGetAfterAGitHubIssuePutReadsTheIssue: as for github-pr, the issue a github-issue put opened is fetched by the get after it.
func TestEndToEndAGetAfterAGitHubIssuePutReadsTheIssue(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: issue
  type: github-issue
  source:
    repo: `+fakeRepo+`
    endpoint: `+fake.URL()+`
jobs:
- name: file
  plan:
  - task: write
    outputs: [files]
    run: |
      echo 'Fix the widgets' > files/title
      echo 'They break.' > files/body.md
  - put: issue
    inputs: [files]
    params:
      title_file: files/title
      body_file: files/body.md
  - get: issue
  - task: read
    inputs: [issue]
    run: echo "[$(cat issue/issue.number)|$(cat issue/issue.title)|$(cat issue/issue.url)]"
`)

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, []string{"run", path, "--job", "file", "--progress", "plain"})
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	created := fake.createdIssues()
	if len(created) != 1 {
		t.Fatalf("opened %+v, want one issue", created)
	}

	want := fmt.Sprintf("[%d|Fix the widgets|%s]", created[0].Number, fake.html("issues", created[0].Number))
	if !strings.Contains(out, want) {
		t.Errorf("the step after the get did not read the issue the put opened, want %s:\n%s", want, out)
	}
}

// TestEndToEndAGetAfterAPutInsideATriggeredBuild: the shape a real job has — a get that starts the build, then a put and the get after it in that build's remainder, where steps count from 0 again — still binds the put's version, and the check runs for the first get only.
func TestEndToEndAGetAfterAPutInsideATriggeredBuild(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	path := writePipeline(t, dir, getAfterPutType(log)+`
- name: source
  type: release
  source: {which: source}
jobs:
- name: publish
  plan:
  - get: source
  - task: prepare
    inputs: []
    run: echo ready
  - put: releases
  - get: releases
  - task: read
    inputs: [releases]
    run: cat releases/ref
    assert:
      stdout: v2
  assert:
    execution: [source, prepare, releases, releases, read]
    outcome: succeeded
`)

	mustRun(t, "run", path, "--job", "publish")

	if got := readLog(t, log); got != "check\nin v1\nout\nin v2\n" {
		t.Errorf("the resources ran %q, want one check and fetch for source, then the put and an in of v2", got)
	}
}

// TestEndToEndAPutInAHookBlockDoesNotReplaceThePlanPut: a hook's block runs its put through the same step runner a plan put uses, and it must not overwrite what the plan's put recorded — or the get after both would find a put from the wrong step and fail.
func TestEndToEndAPutInAHookBlockDoesNotReplaceThePlanPut(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	log := filepath.Join(dir, "log")

	path := writePipeline(t, dir, getAfterPutType(log)+`
jobs:
- name: publish
  plan:
  - put: releases
  - task: work
    inputs: []
    run: echo working
    on_success:
      do:
      - put: releases
  - get: releases
  - task: read
    inputs: [releases]
    run: cat releases/ref
    assert:
      stdout: v2
`)

	mustRun(t, "run", path, "--job", "publish")
}
