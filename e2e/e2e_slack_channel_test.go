package e2e

import (
	"os"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// slackChannelPipeline writes a pipeline whose slack-reply resource is configured by sourceExtra (more source: lines) and returns its path.
func slackChannelPipeline(t *testing.T, baseURL, sourceExtra, jobs string) string {
	t.Helper()

	path := pipelinePath(t, t.TempDir())

	err := os.WriteFile(path, []byte(`
resources:
- name: mentions
  type: slack-mentions
  source:
    base_url: `+baseURL+`
- name: reply
  type: slack-reply
  source:
    base_url: `+baseURL+`
`+sourceExtra+`
jobs:
`+jobs), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return path
}

// TestEndToEndSlackReplyHookDMsSourceChannel: a job-level hook has no inputs and no files, so the channel (a user id DMs) comes from source.channel and the text from params.text.
func TestEndToEndSlackReplyHookDMsSourceChannel(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	path := slackChannelPipeline(t, server.URL, "    channel: UOPERATOR\n", `
- name: build
  plan:
  - task: compile
    run: exit 1
  on_failure:
    put: reply
    params: {text: "build failed, see steps runs"}
`)

	err := runBacklog(t, "run", path, "--job", "build")
	if err == nil {
		t.Fatal("run succeeded, want the failing task to fail the job")
	}

	posted := workspace.postedMessages()
	if len(posted) != 1 {
		t.Fatalf("posted %v, want exactly one message", posted)
	}

	if posted[0]["channel"] != "UOPERATOR" || posted[0]["text"] != "build failed, see steps runs" {
		t.Errorf("posted %v, want the inline text to source.channel", posted[0])
	}

	if _, has := posted[0]["thread_ts"]; has {
		t.Errorf("posted %v, want a top-level message", posted[0])
	}
}

// TestEndToEndSlackReplyChannelPrecedence: an input's version beats the thread/ files, which beat source.channel.
func TestEndToEndSlackReplyChannelPrecedence(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	byInput := slackChannelPipeline(t, server.URL, "    channel: CFALLBACK\n", `
- name: answer
  plan:
  - get: mentions
    trigger: true
  - put: reply
    inputs: [mentions]
    params: {text: "from the thread"}
`)

	mustRunBacklog(t, "run", byInput, "--job", "answer")

	posted := workspace.postedMessages()
	if len(posted) != 1 || posted[0]["channel"] != "C1" || posted[0]["thread_ts"] == nil {
		t.Fatalf("posted %v, want a threaded reply in the mention's channel C1, not source.channel", posted)
	}

	byFile := slackChannelPipeline(t, server.URL, "    channel: CFALLBACK\n", `
- name: announce
  plan:
  - task: address
    outputs: [thread]
    run: printf '%s' "CFILE" > thread/channel
  - put: reply
    inputs: [thread]
    params: {text: "from the file"}
`)

	mustRunBacklog(t, "run", byFile, "--job", "announce")

	posted = workspace.postedMessages()
	if len(posted) != 2 || posted[1]["channel"] != "CFILE" {
		t.Fatalf("posted %v, want the second post in thread/channel's CFILE, not source.channel", posted)
	}
}

// TestEndToEndSlackReplyWithNoDestinationNamesEveryWay: the failure lists all three ways to say where to post.
func TestEndToEndSlackReplyWithNoDestinationNamesEveryWay(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	path := slackChannelPipeline(t, server.URL, "", `
- name: answer
  plan:
  - put: reply
    params: {text: "hello"}
`)

	err := runBacklog(t, "run", path, "--job", "answer")
	if err == nil {
		t.Fatal("run succeeded, want the put to fail with no destination")
	}

	for _, way := range []string{"inputs: [mentions]", "thread/channel", "source.channel"} {
		if !strings.Contains(err.Error(), way) {
			t.Errorf("error does not name %q: %v", way, err)
		}
	}

	if posted := workspace.postedMessages(); len(posted) != 0 {
		t.Errorf("posted %v, want nothing", posted)
	}
}

// TestEndToEndSlackReplyTextRules: params.text is refused empty at load, and a put that also carries answer/reply.md fails rather than picking one.
func TestEndToEndSlackReplyTextRules(t *testing.T) {
	server, workspace := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	empty := slackChannelPipeline(t, server.URL, "    channel: UOPERATOR\n", `
- name: announce
  plan:
  - put: reply
    params: {text: ""}
`)

	err := cli.Run([]string{"validate", empty})
	if err == nil || !strings.Contains(err.Error(), "params.text") {
		t.Fatalf("validate = %v, want the empty params.text refused", err)
	}

	both := slackChannelPipeline(t, server.URL, "    channel: UOPERATOR\n", `
- name: announce
  plan:
  - task: compose
    outputs: [answer]
    run: printf '%s' "from the file" > answer/reply.md
  - put: reply
    inputs: [answer]
    params: {text: "inline"}
`)

	err = runBacklog(t, "run", both, "--job", "announce")
	if err == nil || !strings.Contains(err.Error(), "answer/reply.md") {
		t.Fatalf("run = %v, want the put to refuse two texts", err)
	}

	if posted := workspace.postedMessages(); len(posted) != 0 {
		t.Errorf("posted %v, want nothing", posted)
	}
}
