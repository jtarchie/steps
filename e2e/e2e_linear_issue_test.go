package e2e

import (
	"strings"
	"testing"
)

// linearIssuePipeline opens a ticket from a title and body a task writes, reads it back with the get after the put, and comments on it. params is the put's params: beyond the two files.
func linearIssuePipeline(fake *fakeLinear, params string) string {
	return `
resources:
- name: ticket
  type: linear-issue
  source:
    team: ENG
    endpoint: ` + fake.URL() + `
- name: reply
  type: linear-comment
  source:
    endpoint: ` + fake.URL() + `
jobs:
- name: file
  plan:
  - task: write
    outputs: [files]
    run: |
      echo 'Bump Ruby to 3.4.2' > files/title
      echo 'A patch release.' > files/body.md
  - put: ticket
    inputs: [files]
    params:
      title_file: files/title
      body_file: files/body.md` + params + `
  - get: ticket
  - task: read
    inputs: [ticket]
    outputs: [notes]
    run: |
      echo "[$(cat ticket/issue.identifier)|$(cat ticket/issue.title)|$(cat ticket/issue.url)]"
      echo "picked up by the pipeline" > notes/body.md
      test -s ticket/issue.json && test -s ticket/issue.id && test -s ticket/issue.md
  - put: reply
    inputs: [ticket, notes]
    params: {body_file: notes/body.md}
`
}

func assertCreated(t *testing.T, input, want map[string]any) {
	t.Helper()

	for key, value := range want {
		if input[key] != value {
			t.Errorf("issueCreate %s = %#v, want %#v", key, input[key], value)
		}
	}
}

func runCapturing(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var err error

	out := captureStdout(t, func() {
		err = stepsRun(t, append(args, "--progress", "plain"))
	})

	return out, err
}

// TestEndToEndLinearIssuePutThenGetThenComment is the seam this type exists for: the put opens the issue with every name resolved to Linear's ids, the get after it reads the issue the put made, and a linear-comment finds that issue from the get's input with no wiring.
func TestEndToEndLinearIssuePutThenGetThenComment(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	fake.add(fakeLinearIssue{Identifier: "ENG-7", Title: "Keep dependencies current"})

	path := writePipeline(t, t.TempDir(), linearIssuePipeline(fake, `
      state: In Progress
      labels: [agent-ready, urgent]
      assignee: "@me"
      estimate: 2
      project: platform
      parent: ENG-7`))

	out, err := runCapturing(t, "run", path, "--job", "file")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	created := fake.createdIssues()
	if len(created) != 1 {
		t.Fatalf("issueCreate sent %d times, want once: %+v", len(created), created)
	}

	assertCreated(t, created[0], map[string]any{
		"teamId": "tm-eng", "title": "Bump Ruby to 3.4.2", "description": "A patch release.\n", "stateId": "st-prog",
		"assigneeId": "us-me", "estimate": float64(2), "projectId": "pj-plat", "parentId": "uuid-ENG-7",
	})

	if labels, _ := created[0]["labelIds"].([]any); len(labels) != 2 || labels[0] != "lb-ready" || labels[1] != "ws-urgent" {
		t.Errorf("issueCreate labelIds = %v, want [lb-ready ws-urgent]", created[0]["labelIds"])
	}

	for _, want := range []string{
		"[ENG-201|Bump Ruby to 3.4.2|https://linear.example/acme/issue/ENG-201]",
		`put: ticket (version: {"identifier":"ENG-201"})`,
		"created ENG-201 in team ENG",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run log lacks %q:\n%s", want, out)
		}
	}

	if posted := fake.postedComments(); len(posted) != 1 || posted[0] != "ENG-201: picked up by the pipeline\n" {
		t.Errorf("posted %q, want one comment on ENG-201, the issue the put opened", posted)
	}
}

// TestEndToEndLinearIssueReuseByTitle: with reuse: title, an issue in the team that has the title and is not done or canceled is the put's answer, so a rerun after a later step failed does not open a second ticket. One that is done or canceled is history, not this work, and without reuse nothing is looked for at all.
func TestEndToEndLinearIssueReuseByTitle(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		existing []fakeLinearIssue
		params   string
		want     string // the identifier the get reads
		creates  int
	}{
		"an open match is reused": {
			existing: []fakeLinearIssue{
				{Identifier: "ENG-40", Title: "Bump Ruby to 3.4.2", State: "st-done"},
				{Identifier: "ENG-50", Title: "Bump Ruby to 3.4.2", State: "st-prog"},
				{Identifier: "ENG-60", Title: "bump ruby to 3.4.2"},
			},
			params: "\n      reuse: title",
			want:   "ENG-50",
		},
		"the oldest of two open matches": {
			existing: []fakeLinearIssue{
				{Identifier: "ENG-51", Title: "Bump Ruby to 3.4.2", CreatedAt: "2026-10-02T09:00:00.000Z"},
				{Identifier: "ENG-52", Title: "Bump Ruby to 3.4.2", CreatedAt: "2026-10-03T09:00:00.000Z"},
			},
			params: "\n      reuse: title",
			want:   "ENG-51",
		},
		"done and canceled matches are not": {
			existing: []fakeLinearIssue{
				{Identifier: "ENG-40", Title: "Bump Ruby to 3.4.2", State: "st-done"},
				{Identifier: "ENG-41", Title: "Bump Ruby to 3.4.2", State: "st-canceled"},
			},
			params:  "\n      reuse: title",
			want:    "ENG-201",
			creates: 1,
		},
		"off by default": {
			existing: []fakeLinearIssue{{Identifier: "ENG-50", Title: "Bump Ruby to 3.4.2", State: "st-prog"}},
			want:     "ENG-201",
			creates:  1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeLinear(t)
			for _, issue := range tc.existing {
				fake.add(issue)
			}

			path := writePipeline(t, t.TempDir(), linearIssuePipeline(fake, tc.params))

			out, err := runCapturing(t, "run", path, "--job", "file")
			if err != nil {
				t.Fatalf("run: %v\n%s", err, out)
			}

			if got := len(fake.createdIssues()); got != tc.creates {
				t.Errorf("issueCreate sent %d times, want %d", got, tc.creates)
			}

			if !strings.Contains(out, "["+tc.want+"|") {
				t.Errorf("the get did not read %s:\n%s", tc.want, out)
			}

			for _, line := range []string{"reused " + tc.want + ": it is open in team ENG", "  reused: yes"} {
				if strings.Contains(out, line) != (tc.creates == 0) {
					t.Errorf("run log says %q: %v, want %v:\n%s", line, strings.Contains(out, line), tc.creates == 0, out)
				}
			}

			if posted := fake.postedComments(); len(posted) != 1 || !strings.HasPrefix(posted[0], tc.want+": ") {
				t.Errorf("posted %q, want the comment on %s", posted, tc.want)
			}
		})
	}
}

// TestEndToEndLinearIssueBumpsShape is the motivating pipeline's ticket, expressed with the built-in: a custom state, workspace labels whose names have spaces, the key's owner, no estimate, find-or-create by title. Run twice, it opens one ticket.
func TestEndToEndLinearIssueBumpsShape(t *testing.T) {
	t.Parallel()

	fake := newFakeLinear(t)
	fake.states = append(fake.states, fakeLinearState{"st-started", "Started", "started"})
	fake.labels["ws-dev"] = "Dev Investment"
	fake.labels["ws-chore"] = "Chore"

	path := writePipeline(t, t.TempDir(), linearIssuePipeline(fake, `
      state: Started
      labels: [Dev Investment, Chore]
      assignee: "@me"
      reuse: title`))

	for range 2 {
		out, err := runCapturing(t, "run", path, "--job", "file")
		if err != nil {
			t.Fatalf("run: %v\n%s", err, out)
		}

		if !strings.Contains(out, `get: ticket (version: {"identifier":"ENG-201"}, recorded by put ticket)`) {
			t.Fatalf("the get did not fetch ENG-201:\n%s", out)
		}
	}

	created := fake.createdIssues()
	if len(created) != 1 {
		t.Fatalf("issueCreate sent %d times over two runs, want once", len(created))
	}

	if _, ok := created[0]["estimate"]; ok {
		t.Errorf("issueCreate sent an estimate %v, want none", created[0]["estimate"])
	}

	if created[0]["stateId"] != "st-started" || created[0]["assigneeId"] != "us-me" {
		t.Errorf("issueCreate = %+v, want Started and the key's own user", created[0])
	}
}

// TestEndToEndLinearIssueRefusesWhatLinearDoesNotHave: every name is looked up before the one request that creates, so a typo anywhere is a red put by name and no issue.
func TestEndToEndLinearIssueRefusesWhatLinearDoesNotHave(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ params, want string }{
		"unknown state":    {params: "\n      state: Shipped", want: `params.state: team ENG has no state named "Shipped"`},
		"unknown label":    {params: "\n      labels: [urgent, agent-redy]", want: `params.labels: Linear has no label named "agent-redy"`},
		"unknown project":  {params: "\n      project: Platforms", want: `params.project: team ENG has no project named "Platforms"`},
		"unknown parent":   {params: "\n      parent: ENG-999", want: "params.parent: no Linear issue ENG-999"},
		"unknown assignee": {params: "\n      assignee: us-nobody", want: `params.assignee: Linear has no user "us-nobody"`},
		"label under reuse": {
			params: "\n      labels: [agent-redy]\n      reuse: title",
			want:   `params.labels: Linear has no label named "agent-redy"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeLinear(t)
			fake.add(fakeLinearIssue{Identifier: "ENG-50", Title: "Bump Ruby to 3.4.2"})

			path := writePipeline(t, t.TempDir(), linearIssuePipeline(fake, tc.params))

			err := stepsRun(t, []string{"run", path, "--job", "file"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want it to say %q", err, tc.want)
			}

			if got := len(fake.createdIssues()); got != 0 {
				t.Errorf("a refused put still sent issueCreate %d times", got)
			}
		})
	}
}

// TestEndToEndLinearIssueRefusesItsFiles: the title is one line and the body is not empty, as for github-issue, checked before any request.
func TestEndToEndLinearIssueRefusesItsFiles(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ write, want string }{
		"two-line title": {write: "printf 'one\\ntwo\\n' > files/title; echo body > files/body.md", want: "a title is one line"},
		"empty body":     {write: "echo title > files/title; : > files/body.md", want: "params.body_file: files/body.md is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			fake := newFakeLinear(t)
			pipeline := strings.Replace(linearIssuePipeline(fake, ""),
				"      echo 'Bump Ruby to 3.4.2' > files/title\n      echo 'A patch release.' > files/body.md\n",
				"      "+tc.write+"\n", 1)

			path := writePipeline(t, t.TempDir(), pipeline)

			err := stepsRun(t, []string{"run", path, "--job", "file"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run = %v, want it to say %q", err, tc.want)
			}

			if got := len(fake.createdIssues()); got != 0 {
				t.Errorf("issueCreate sent %d times", got)
			}
		})
	}
}
