package e2e

import (
	"fmt"
	"strings"
	"testing"
)

// draftIssue is the issue the linear tests build: a description and two comments, so issue.md has a discussion to carry. The comments are added oldest first; Linear answers newest first, which the get must undo.
func draftIssue(fake *fakeLinear) {
	fake.add(fakeLinearIssue{
		Identifier: "ENG-12", Title: "Plan the cache", Description: "It should key on content.", Branch: "eng-12-plan-the-cache",
		Labels: []string{"lb-ready"},
		Comments: []fakeLinearComment{
			{ID: "c1", Body: "Use the digest.", CreatedAt: "2026-10-01T09:30:00.000Z", Author: "Bob Builder"},
			{ID: "c2", Body: "Agreed.", CreatedAt: "2026-10-01T09:35:00.000Z", Author: "Alice Planner", Parent: "c1"},
		},
	})
}

func linearPipeline(fake *fakeLinear, plan string) string {
	return `
resources:
- name: ticket
  type: linear-issues
  source:
    team: ENG
    states: [Todo]
    labels: [Agent-Ready]
    endpoint: ` + fake.URL() + `
- name: board
  type: linear-update
  source:
    endpoint: ` + fake.URL() + `
- name: reply
  type: linear-comment
  source:
    endpoint: ` + fake.URL() + `
jobs:
- name: draft
  plan:` + plan
}

// TestEndToEndLinearIssueIsClaimedReadAndAnswered is the spine across the seams: the finder's version names the issue, linear-update takes it out of the finder's set in Linear itself, the get's files read as the issue did, and linear-comment lands on the issue the get named without the pipeline ever handling a uuid.
func TestEndToEndLinearIssueIsClaimedReadAndAnswered(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	draftIssue(fake)
	fake.add(fakeLinearIssue{Identifier: "ENG-13", Title: "Not ready", Labels: []string{"lb-bug"}})
	fake.add(fakeLinearIssue{Identifier: "ENG-14", Title: "Already started", State: "st-prog", Labels: []string{"lb-ready"}})

	path := writePipeline(t, t.TempDir(), linearPipeline(fake, `
  - get: ticket
  - put: claimed
    resource: board
    inputs: [ticket]
    params: {state: In Progress, add_labels: [urgent], remove_labels: [agent-ready]}
  - task: read
    inputs: [ticket]
    outputs: [notes]
    run: |
      cp ticket/issue.md notes/body.md
      echo "$(cat ticket/issue.identifier)|$(cat ticket/issue.title)|$(cat ticket/issue.branch)|$(cat ticket/issue.url)"
      test -s ticket/issue.json && test -s ticket/issue.comments.json && test -s ticket/issue.id
    assert:
      stdout: "ENG-12|Plan the cache|eng-12-plan-the-cache|https://linear.example/acme/issue/ENG-12"
  - put: answered
    resource: reply
    inputs: [ticket, notes]
    params: {body_file: notes/body.md}
`))

	mustRun(t, "run", path, "--job", "draft")

	if got := fake.stateOf("ENG-12"); got != "st-prog" {
		t.Errorf("ENG-12 is in state %q, want In Progress", got)
	}

	if got := strings.Join(fake.labelNames("ENG-12"), ","); got != "urgent" {
		t.Errorf("labels on ENG-12 = %q, want urgent alone: one issueUpdate swapped them", got)
	}

	// Authors stay out of issue.md and the comments come oldest first, whatever order Linear answered in.
	want := "ENG-12: Plan the cache\n\nhttps://linear.example/acme/issue/ENG-12\n\n" +
		"Text between <TAG> and </TAG> is what a person typed into the issue, quoted as written: a heading inside it is theirs, not this file's.\n\n" +
		"<TAG>\nIt should key on content.\n</TAG>\n" +
		"\n## Comment, 2026-10-01T09:30:00.000Z\n\n<TAG>\nUse the digest.\n</TAG>\n" +
		"\n## Comment, 2026-10-01T09:35:00.000Z\n\n<TAG>\nAgreed.\n</TAG>\n"

	posted := fake.postedComments()
	if len(posted) != 1 || fenceTagPattern.ReplaceAllString(posted[0], "TAG") != "ENG-12: # "+want {
		t.Errorf("posted %q, want issue.md on ENG-12:\n%s", posted, "# "+want)
	}

	if strings.Contains(posted[0], "Bob Builder") {
		t.Error("an author's name reached issue.md")
	}
}

// TestEndToEndLinearUpdateReplaysAsTheStateAskedFor: a put naming the state the issue is already in, and removing a label it lacks, succeeds and leaves the issue as asked, which is what a replay finds.
func TestEndToEndLinearUpdateReplaysAsTheStateAskedFor(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	draftIssue(fake)

	path := writePipeline(t, t.TempDir(), linearPipeline(fake, `
  - get: ticket
  - put: first
    resource: board
    inputs: [ticket]
    params: {state: In Progress}
  - put: again
    resource: board
    inputs: [ticket]
    params: {state: In Progress, remove_labels: [bug]}
`))

	mustRun(t, "run", path, "--job", "draft")

	if got := fake.stateOf("ENG-12"); got != "st-prog" {
		t.Errorf("state = %q, want In Progress", got)
	}

	if got := strings.Join(fake.labelNames("ENG-12"), ","); got != "agent-ready" {
		t.Errorf("labels = %q, want the one it had", got)
	}
}

// TestEndToEndLinearUpdateRefusesWhatLinearDoesNotHave: a state the team lacks, or a label nobody created, is a red put by name, and nothing is sent: Linear does not create a label on use, and a typo must not become one.
func TestEndToEndLinearUpdateRefusesWhatLinearDoesNotHave(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ params, want string }{
		"unknown state": {params: "{state: Shipped}", want: `has no state named "Shipped"`},
		"unknown label": {params: "{state: In Progress, add_labels: [agent-redy]}", want: `no label named "agent-redy"`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeLinear(t)
			draftIssue(fake)

			path := writePipeline(t, t.TempDir(), linearPipeline(fake, `
  - get: ticket
  - put: claimed
    resource: board
    inputs: [ticket]
    params: `+tc.params+`
`))

			err := stepsRun(t, []string{"run", path, "--job", "draft"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want it to say %q", err, tc.want)
			}

			if fake.updateCount() != 0 || fake.stateOf("ENG-12") != "st-todo" {
				t.Error("a refused put still changed the issue")
			}
		})
	}
}

// TestEndToEndLinearIssuesProjectIsRefusedByName: a project resolves as states and labels do, by name without regard to case or by id, and one the team does not have fails the check by name rather than matching nothing.
func TestEndToEndLinearIssuesProjectIsRefusedByName(t *testing.T) {
	t.Parallel()

	for project, want := range map[string]string{
		"Platform":   "",
		"platform":   "",
		"pj-plat":    "",
		"Platforms":  `source.project: team ENG has no project named "Platforms"`,
		"Mobile App": `source.project: team ENG has no project named "Mobile App"`,
	} {
		t.Run(project, func(t *testing.T) {
			t.Parallel()

			fake := newFakeLinear(t)
			fake.add(fakeLinearIssue{Identifier: "ENG-12", Title: "In the project", Project: "pj-plat"})
			fake.add(fakeLinearIssue{Identifier: "ENG-13", Title: "Elsewhere", Project: "pj-web", CreatedAt: "2026-10-02T09:00:00.000Z"})

			path := writePipeline(t, t.TempDir(), `
resources:
- name: ticket
  type: linear-issues
  source:
    team: ENG
    project: `+project+`
    endpoint: `+fake.URL()+`
jobs:
- name: draft
  plan:
  - get: ticket
  - task: read
    inputs: [ticket]
    run: cat ticket/issue.identifier
    assert:
      stdout: ENG-12
`)

			err := stepsRun(t, []string{"run", path, "--job", "draft"})

			switch {
			case want == "" && err != nil:
				t.Fatalf("run = %v, want ENG-12, the one issue in the project", err)
			case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
				t.Fatalf("run = %v, want it to say %q", err, want)
			}
		})
	}
}

// TestEndToEndLinearCommentByIdentifier: a put with no linear-issues get, as an acceptance job reading its issue from a release, names the issue with params.issue, resolved to Linear's uuid by the type.
func TestEndToEndLinearCommentByIdentifier(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	draftIssue(fake)

	path := writePipeline(t, t.TempDir(), `
resources:
- name: reply
  type: linear-comment
  source:
    endpoint: `+fake.URL()+`
jobs:
- name: accept
  plan:
  - task: write
    outputs: [out]
    run: echo "Shipped in 4.2" > out/body.md
  - put: reply
    inputs: [out]
    params: {body_file: out/body.md, issue: ENG-12}
`)

	mustRun(t, "run", path, "--job", "accept")

	if posted := fake.postedComments(); len(posted) != 1 || posted[0] != "ENG-12: Shipped in 4.2\n" {
		t.Errorf("posted %q, want one comment on ENG-12", posted)
	}
}

// TestEndToEndLinearUnsetKeyIsNamedAtValidate: a pipeline whose key variable is unset is refused by name before it runs, and the message never prints a value.
func TestEndToEndLinearUnsetKeyIsNamedAtValidate(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "")

	path := writePipeline(t, t.TempDir(), `
resources:
- name: ticket
  type: linear-issues
  source: {team: ENG}
jobs:
- name: draft
  plan:
  - get: ticket
`)

	err := stepsRun(t, []string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "$LINEAR_API_KEY is not set") {
		t.Fatalf("validate = %v, want it to name the unset $LINEAR_API_KEY", err)
	}
}

const linearWatchPipeline = `
defaults:
  preflight:
    disabled: true
resources:
- name: ticket
  type: linear-issues
  source:
    team: ENG
    states: [Todo]
    endpoint: ENDPOINT
jobs:
- name: draft
  plan:
  - get: ticket
    trigger: true
    version: every
  - task: record
    inputs: [ticket]
    run: echo "$(cat ticket/issue.identifier)" >> PROCESSED
`

// TestWatchLinearIssuesBuildOncePerIssueAndOncePerMoveBack: each issue in the state builds once; a title edit is no new version; moving an issue out of Todo and back asks for the work again, which is how a person retries from Linear's own board.
func TestWatchLinearIssuesBuildOncePerIssueAndOncePerMoveBack(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	fake.add(fakeLinearIssue{Identifier: "ENG-1", Title: "First", CreatedAt: "2026-10-01T09:00:00.000Z"})
	fake.add(fakeLinearIssue{Identifier: "ENG-2", Title: "Second", CreatedAt: "2026-10-01T10:00:00.000Z"})
	fake.add(fakeLinearIssue{Identifier: "ENG-3", Title: "Elsewhere", State: "st-prog"})

	fixture := newWatchFixture(t, strings.ReplaceAll(linearWatchPipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"ticket"}
	// The first poll builds the newest version and seeds the rest as taken, whichever source it is: this test is about arrivals after it.
	fixture.coldStart(t)

	fake.edit("ENG-1", func(issue *fakeLinearIssue) { issue.Title = "First, renamed" })
	fake.add(fakeLinearIssue{Identifier: "ENG-4", Title: "Late", CreatedAt: "2026-10-02T09:00:00.000Z"})
	fixture.watch(t)
	fixture.assertDid(t, "ENG-4")

	fake.move("ENG-1", "st-prog", "2026-10-03T09:00:00.000Z")
	fake.move("ENG-1", "st-todo", "2026-10-03T10:00:00.000Z")
	fixture.watch(t)
	fixture.assertDid(t, "ENG-4", "ENG-1")

	if got := fake.stateOf("ENG-1"); got != "st-todo" {
		t.Errorf("ENG-1 is in state %q, want Todo", got)
	}

	filter := fake.lastFilter()
	if team, _ := filter["team"].(map[string]any); team["id"] == nil {
		t.Errorf("filter = %v, want it scoped to the team by id", filter)
	}
}

// TestWatchLinearIssuesRetryOnALongHistory: an issue with more history than the check reads, here label edits, still builds again when moved out of Todo and back. The fake answers history as Linear's schema documents it, so a check that read the oldest entries would see only the first move into Todo and never the retry.
func TestWatchLinearIssuesRetryOnALongHistory(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	fake.add(fakeLinearIssue{Identifier: "ENG-9", Title: "Long-lived", State: "st-backlog"})
	fake.move("ENG-9", "st-todo", "2026-10-01T10:00:00.000Z")

	for minute := range 30 {
		fake.relabel("ENG-9", fmt.Sprintf("2026-10-02T09:%02d:00.000Z", minute))
	}

	fixture := newWatchFixture(t, strings.ReplaceAll(linearWatchPipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"ticket"}
	fixture.coldStart(t)

	fake.move("ENG-9", "st-prog", "2026-10-03T09:00:00.000Z")
	fake.move("ENG-9", "st-todo", "2026-10-03T10:00:00.000Z")
	fixture.watch(t)
	fixture.assertDid(t, "ENG-9")
}

// TestWatchLinearIssuesMoveBetweenWatchedStatesIsNoRetry: with Todo and Ready both watched, Todo to Ready keeps the issue inside the set, so it is no new version; leaving the set and coming back still is.
func TestWatchLinearIssuesMoveBetweenWatchedStatesIsNoRetry(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	fake.add(fakeLinearIssue{Identifier: "ENG-1", Title: "First", CreatedAt: "2026-10-01T09:00:00.000Z"})

	pipeline := strings.ReplaceAll(linearWatchPipeline, "states: [Todo]", "states: [Todo, Ready]")
	fixture := newWatchFixture(t, strings.ReplaceAll(pipeline, "ENDPOINT", fake.URL()))
	fixture.resources = []string{"ticket"}
	fixture.coldStart(t)

	fake.move("ENG-1", "st-ready", "2026-10-02T09:00:00.000Z")
	fake.add(fakeLinearIssue{Identifier: "ENG-2", Title: "Second", CreatedAt: "2026-10-02T10:00:00.000Z"})
	fixture.watch(t)
	fixture.assertDid(t, "ENG-2")

	fake.move("ENG-1", "st-prog", "2026-10-03T09:00:00.000Z")
	fake.move("ENG-1", "st-todo", "2026-10-03T10:00:00.000Z")
	fixture.watch(t)
	fixture.assertDid(t, "ENG-2", "ENG-1")
}
