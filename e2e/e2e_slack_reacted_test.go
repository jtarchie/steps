package e2e

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndBuiltinSlackReactionWatch is the get side of slack-reaction:
// react to a message with an emoji and a build answers it. Through the whole
// CLI stack against the fake workspace, it pins what a watcher has to get
// right:
//
//   - a reaction counts only when the token's user left it: D1/50.000 carries
//     the emoji from someone else, and a 👍 from us, so it is listed by
//     reactions.list and must still be skipped;
//   - the done emoji, from us, means answered: C1/101.000 carries both and is
//     never reported;
//   - a reacted thread REPLY is answered in its thread and marked on itself,
//     the same split slack-mentions makes, because the version has the same
//     shape and the two puts read it unchanged;
//   - limit: 1 forces every item onto its own page, so C1/100.000, the oldest
//     reacted message and the last page, is found only if the check follows
//     next_cursor.
func TestEndToEndBuiltinSlackReactionWatch(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	workspace.tag("C1", "100.000", "robot_face", "UBOT")
	workspace.tag("C1", "101.500", "robot_face", "UBOT", "U2")
	workspace.tag("D1", "50.000", "robot_face", "U2")
	workspace.tag("D1", "50.000", "thumbsup", "UBOT")
	workspace.tag("C1", "101.000", "robot_face", "UBOT")
	workspace.tag("C1", "101.000", "white_check_mark", "UBOT")

	path := pipelinePath(t, t.TempDir())

	err := os.WriteFile(path, []byte(`
resources:
- name: tagged
  type: slack-reaction
  source:
    base_url: `+server.URL+`
    emoji: robot_face
    done: white_check_mark
    limit: 1
- name: reply
  type: slack-reply
  source:
    base_url: `+server.URL+`
- name: mark
  type: slack-reaction
  source:
    base_url: `+server.URL+`

jobs:
- name: answer
  plan:
  - get: tagged
    trigger: true
    version: every
  - task: compose
    inputs: [tagged]
    outputs: [answer]
    run: |
      printf 'read %s, tagged %s, from %s' \
        "$(grep -c '"ts":' tagged/thread.json)" \
        "$(grep -o '"ts": "[0-9.]*"' tagged/message.json | cut -d'"' -f4)" \
        "$(cat tagged/user)" > answer/reply.md
  - put: reply
    inputs: [tagged, answer]
  - put: mark
    inputs: [tagged]
    params: {add: white_check_mark}
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)
	mustRunBacklog(t, "run", path, "--job", "answer")

	got := map[string]string{}

	for _, p := range workspace.postedMessages() {
		channel, _ := p["channel"].(string)
		ts, _ := p["thread_ts"].(string)
		text, _ := p["text"].(string)

		got[channel+"/"+ts] = text
	}

	want := map[string]string{
		"C1/100.000": "read 1, tagged 100.000, from U2",
		"C1/101.000": "read 2, tagged 101.500, from U3",
	}

	if !maps.Equal(got, want) {
		t.Errorf("posted = %v, want %v", got, want)
	}

	calls := workspace.reactions()
	marked := make([]string, 0, len(calls))

	for _, call := range calls {
		channel, _ := call["channel"].(string)
		ts, _ := call["timestamp"].(string)
		name, _ := call["name"].(string)
		endpoint, _ := call["endpoint"].(string)

		marked = append(marked, endpoint+" "+name+" "+channel+"/"+ts)
	}

	slices.Sort(marked)

	wantMarked := []string{
		"/api/reactions.add white_check_mark C1/100.000",
		"/api/reactions.add white_check_mark C1/101.500",
	}
	if !slices.Equal(marked, wantMarked) {
		t.Errorf("reactions = %v, want %v: the mark goes on the reacted message, never the thread parent", marked, wantMarked)
	}

	if workspace.reactionPagesAsked() < 4 {
		t.Errorf("reactions.list pages asked = %d, want at least 4 (one item per page)", workspace.reactionPagesAsked())
	}
}

// TestEndToEndBuiltinSlackReactionWatchPages: pages caps how far back a
// check reads, and the done guard stops it at the last page. Without the cap
// a workspace with many reacted messages costs every page there is; without
// the guard a short list costs pages requests anyway.
func TestEndToEndBuiltinSlackReactionWatchPages(t *testing.T) {
	for name, tc := range map[string]struct {
		tagged []string
		pages  string
		want   int
	}{
		"capped by pages":        {tagged: []string{"100.000", "101.000", "101.500"}, pages: "2", want: 2},
		"stops at the last page": {tagged: []string{"100.000", "101.000"}, pages: "5", want: 2},
	} {
		t.Run(name, func(t *testing.T) {
			server, workspace := fakeSlack(t)
			t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

			for _, ts := range tc.tagged {
				workspace.tag("C1", ts, "robot_face", "UBOT")
			}

			path := pipelinePath(t, t.TempDir())

			err := os.WriteFile(path, []byte(`
resources:
- name: tagged
  type: slack-reaction
  source:
    base_url: `+server.URL+`
    emoji: robot_face
    limit: 1
    pages: `+tc.pages+`

jobs:
- name: look
  plan:
  - get: tagged
    trigger: true
`), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			mustRun(t, "run", path, "--job", "look")

			if workspace.reactionPagesAsked() != tc.want {
				t.Errorf("reactions.list pages asked = %d, want exactly %d", workspace.reactionPagesAsked(), tc.want)
			}
		})
	}
}

// TestEndToEndBuiltinSlackReactionWatchLongThread: a thread past in:'s 1000
// messages is cut at its newest end, which is where a fresh reaction usually
// is. message.json must still be the reacted message, fetched on its own.
func TestEndToEndBuiltinSlackReactionWatchLongThread(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	for i := range 1001 {
		workspace.say(slackMsg{channel: "C1", ts: strconv.Itoa(200+i) + ".000", user: "U2", text: "more", parent: "101.000"})
	}

	workspace.say(slackMsg{channel: "C1", ts: "5000.000", user: "U3", text: "the ask", parent: "101.000"})
	workspace.tag("C1", "5000.000", "robot_face", "UBOT")

	path := pipelinePath(t, t.TempDir())

	err := os.WriteFile(path, []byte(`
resources:
- name: tagged
  type: slack-reaction
  source:
    base_url: `+server.URL+`
    emoji: robot_face

jobs:
- name: look
  plan:
  - get: tagged
    trigger: true
  - task: read
    inputs: [tagged]
    run: |
      grep -q '"text": "the ask"' tagged/message.json
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "look")
}

// TestEndToEndBuiltinSlackReactionWatchOldestFirst: a check owes its
// versions oldest first, and reactions.list answers newest first, so without
// the sort a plain get builds the OLDEST reacted message as the latest.
func TestEndToEndBuiltinSlackReactionWatchOldestFirst(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	workspace.tag("C1", "100.000", "robot_face", "UBOT")
	workspace.tag("C1", "101.500", "robot_face", "UBOT")

	path := pipelinePath(t, t.TempDir())

	err := os.WriteFile(path, []byte(`
resources:
- name: tagged
  type: slack-reaction
  source:
    base_url: `+server.URL+`
    emoji: robot_face

jobs:
- name: look
  plan:
  - get: tagged
    trigger: true
  - task: read
    inputs: [tagged]
    run: |
      grep -q '"ts": "101.500"' tagged/version.json
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "look")
}

// TestEndToEndBuiltinSlackReactionWatchFailsOnRefusal: Slack refuses with 200
// and ok:false. A check that read that as an empty page would report nothing,
// forever, which looks exactly like nobody having reacted to anything.
func TestEndToEndBuiltinSlackReactionWatchFailsOnRefusal(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	workspace.mu.Lock()
	workspace.reactionError = map[string]string{"/api/reactions.list": "missing_scope"}
	workspace.mu.Unlock()

	path := pipelinePath(t, t.TempDir())

	err := os.WriteFile(path, []byte(`
resources:
- name: tagged
  type: slack-reaction
  source:
    base_url: `+server.URL+`
    emoji: robot_face

jobs:
- name: look
  plan:
  - get: tagged
    trigger: true
`), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = cli.Run([]string{"run", path, "--job", "look"})
	if err == nil || !strings.Contains(err.Error(), "slack reactions.list: missing_scope") {
		t.Fatalf("run = %v, want the refusal named", err)
	}
}

// TestSlackReactionWatchRefusedAtLoad pins the two shapes a watcher cannot
// take. A get with no source.emoji has nothing to watch for. A put to a
// resource that watches would record its own version on that resource, which
// the watching get then fetches as new: the mark answers the message again.
func TestSlackReactionWatchRefusedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"get without emoji": {`
resources:
- name: tagged
  type: slack-reaction
  source: {}
jobs:
- name: j
  plan:
  - get: tagged
`, "source.emoji is required"},
		"put to a watcher": {`
resources:
- name: tagged
  type: slack-reaction
  source: {emoji: robot_face}
jobs:
- name: j
  plan:
  - get: tagged
  - put: tagged
    inputs: [tagged]
    params: {add: eyes}
`, "mark through a second slack-reaction resource"},
	} {
		t.Run(name, func(t *testing.T) {
			path := pipelinePath(t, t.TempDir())

			err := os.WriteFile(path, []byte(tc.yaml), 0o600)
			if err != nil {
				t.Fatal(err)
			}

			err = cli.Run([]string{"validate", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validate = %v, want %q", err, tc.want)
			}
		})
	}
}
