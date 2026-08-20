package main

// What a first-ever check of slack-mentions costs.
//
// The cursor is what keeps the thread fan-out small: a thread is only asked
// about when its latest_reply is newer than the last version reported. On a
// cold start there is no cursor, `since` is 0, and EVERY thread with any
// reply qualifies — one conversations.replies call each, across the last
// `limit` messages of every channel the bot is in.
//
// That is not merely slow, it is a trap that cannot open itself. The check
// fails the whole poll on a rate-limited call (deliberately — see the note on
// tolerating them in slack-mentions.yml), a failed check reports no version,
// no version means no cursor, and the next poll does exactly the same thing.
// A workspace with more threads than the rate limit allows calls for is a
// watcher that never runs once.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coldStartThreadBudget is the ceiling slack-mentions.yml holds itself to on a
// first check. Named here rather than inlined so the test says what it is
// asserting rather than testing a magic number.
const coldStartThreadBudget = 5

// TestEndToEndBuiltinSlackColdStartBoundsItsThreadFanOut is the regression:
// a busy workspace's first check must not ask about every thread it can see.
func TestEndToEndBuiltinSlackColdStartBoundsItsThreadFanOut(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	// Twenty threads, none of them mentioning the bot, each with a reply — a
	// channel that has simply been in use for a while. Under the old rule all
	// twenty qualified, because latest_reply > 0 is true of every thread that
	// has ever been replied to.
	for i := range 20 {
		parent := fmt.Sprintf("%d.000", 200+i)
		workspace.say(slackMsg{channel: "C1", ts: parent, user: "U2", text: "ordinary chatter"})
		workspace.say(slackMsg{
			channel: "C1",
			ts:      fmt.Sprintf("%d.500", 200+i),
			user:    "U3",
			text:    "ordinary reply",
			parent:  parent,
		})
	}

	mustRun(t, "run", coldStartPipeline(t, server.URL), "--job", "answer")

	asked := workspace.threadsAsked()
	if len(asked) > coldStartThreadBudget {
		t.Errorf("a cold start asked about %d threads, want at most %d: %v",
			len(asked), coldStartThreadBudget, asked)
	}
}

// TestEndToEndBuiltinSlackColdStartStillAnswersTheNewestThreadMention is the
// other half, and the reason the bound is a bound rather than zero: the
// mention people actually write is a reply inside a thread, and the newest one
// still has to be found. A cold start that asked about nothing would be cheap
// and useless.
func TestEndToEndBuiltinSlackColdStartStillAnswersTheNewestThreadMention(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	// Older chatter than the fixture's own thread, so the mention buried at
	// 101.500 stays among the most recently active and must survive the trim.
	for i := range 12 {
		parent := fmt.Sprintf("%d.000", 10+i)
		workspace.say(slackMsg{channel: "C1", ts: parent, user: "U2", text: "old chatter"})
		workspace.say(slackMsg{
			channel: "C1",
			ts:      fmt.Sprintf("%d.500", 10+i),
			user:    "U3",
			text:    "old reply",
			parent:  parent,
		})
	}

	mustRun(t, "run", coldStartPipeline(t, server.URL), "--job", "answer")

	posted := workspace.postedMessages()

	// 101.000 is the PARENT of the in-thread mention at 101.500 — a reply is
	// threaded on its parent, never on its own ts.
	answered := false

	for _, message := range posted {
		if channel, _ := message["channel"].(string); channel != "C1" {
			continue
		}

		if ts, _ := message["thread_ts"].(string); ts == "101.000" {
			answered = true
		}
	}

	if !answered {
		t.Errorf("a cold start did not answer the mention buried in the newest thread: %v", posted)
	}
}

// coldStartPipeline writes the smallest pipeline that both discovers mentions
// and answers them, so the tests above can assert on what Slack was asked as
// well as what it was told.
func coldStartPipeline(t *testing.T, baseURL string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pipeline.yml")

	body := strings.ReplaceAll(`
resources:
- name: mentions
  type: slack-mentions
  source:
    base_url: BASE
- name: reply
  type: slack-reply
  source:
    base_url: BASE

jobs:
- name: answer
  plan:
  - get: mentions
    trigger: true
    version: every
  - task: address
    inputs: [mentions]
    outputs: [thread, answer]
    run: |
      set -eu
      grep -o '"channel": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/channel
      grep -o '"thread_ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      printf 'answered' > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`, "BASE", baseURL)

	err := os.WriteFile(path, []byte(body), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}
