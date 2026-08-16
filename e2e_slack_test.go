package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fakeSlack serves just enough of Slack's web API for the built-in
// slack-mentions/slack-reply resource types: auth.test resolves the bot's own
// id, conversations.history/replies answer from a small fixed transcript, and
// chat.postMessage records what was posted.
//
// The transcript is shaped to exercise both branches a real check has to get
// right: a top-level message that mentions the bot, an unrelated top-level
// message whose only relevance is that it has a reply, and a reply — buried
// in that thread — that mentions the bot. A channel_join system message also
// contains the mention text ("<@UBOT> has joined"), which must NOT count.
//
// D1 is a 1:1 DM (Slack's own "D" id prefix): its one message names no
// mention at all, and must still be picked up — the DM-doesn't-need-a-
// mention branch. C1 stays mention-gated throughout, so a DM-shaped message
// without a mention landing there would prove the branch leaked past its
// channel.
func fakeSlack(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()

	return fakeSlackRateLimiting(t, nil)
}

// fakeSlackRateLimiting is fakeSlack with a budget of 429s to serve first, per
// API path — how a real workspace answers when a watch host polls it faster
// than its rate limit allows.
func fakeSlackRateLimiting(t *testing.T, rateLimited map[string]int) (*httptest.Server, *[]map[string]any) {
	t.Helper()

	var mu sync.Mutex

	var posted []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remaining := rateLimited[r.URL.Path]

		if remaining > 0 {
			rateLimited[r.URL.Path] = remaining - 1
		}
		mu.Unlock()

		if remaining > 0 {
			// Retry-After: 0 so the test does not actually wait out a backoff.
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/api/auth.test":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "user_id": "UBOT"})
		case "/api/users.conversations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"channels": []map[string]string{{"id": "C1"}, {"id": "D1"}},
			})
		case "/api/conversations.history":
			messages := fakeSlackHistory(r.URL.Query().Get("oldest"), r.URL.Query().Get("channel"))

			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
		case "/api/conversations.replies":
			ts := r.URL.Query().Get("ts")

			messages := []map[string]any{{"type": "message", "user": "U2", "ts": ts, "text": "parent"}}
			if ts == "101.000" {
				messages = []map[string]any{
					{"type": "message", "user": "U2", "ts": "101.000", "text": "no mention here"},
					{"type": "message", "user": "U3", "ts": "101.500", "text": "actually <@UBOT> can you look?"},
				}
			}

			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": messages})
		case "/api/chat.postMessage":
			body, _ := io.ReadAll(r.Body)

			var payload map[string]any

			_ = json.Unmarshal(body, &payload)

			mu.Lock()
			posted = append(posted, payload)
			mu.Unlock()

			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": payload["channel"], "ts": "999.999"})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "unknown_method"})
		}
	}))
	t.Cleanup(server.Close)

	return server, &posted
}

// fakeSlackHistory returns conversations.history's fixed transcript: nothing
// on any poll after the first (oldest != "0"), D1's unmentioned DM message,
// or C1's channel transcript otherwise.
func fakeSlackHistory(oldest, channel string) []map[string]any {
	if oldest != "0" {
		return []map[string]any{}
	}

	if channel == "D1" {
		return []map[string]any{
			{"type": "message", "user": "U2", "ts": "50.000", "text": "no bot mention needed, this is a DM"},
		}
	}

	return []map[string]any{
		{"type": "message", "user": "U2", "ts": "100.000", "text": "hey <@UBOT> help?"},
		{"type": "message", "user": "U2", "ts": "101.000", "text": "no mention here", "reply_count": 1},
		{"type": "message", "subtype": "channel_join", "user": "U2", "ts": "102.000", "text": "<@UBOT> has joined the channel"},
	}
}

// TestEndToEndBuiltinSlackMentionsAndReply drives both built-in types through
// the whole CLI stack against a fake Slack API: check finds both mentions (one
// top-level, one buried in a thread the parent message doesn't itself
// mention), version: every fans the job out once per mention, and each fetched
// thread is answered in place — proving the two types compose the way the
// hand-written pipeline they were extracted from does.
func TestEndToEndBuiltinSlackMentionsAndReply(t *testing.T) {
	server, posted := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resources:
- name: mentions
  type: slack-mentions
  source:
    base_url: ` + server.URL + `
- name: reply
  type: slack-reply
  source:
    base_url: ` + server.URL + `

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
      grep -o '"ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      printf '%s' "answered" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)
	mustRun(t, "run", path, "--job", "answer")

	if len(*posted) != 3 {
		t.Fatalf("posted %d messages, want 3 (two channel mentions + one unmentioned DM): %v", len(*posted), *posted)
	}

	// Two replies land in channel C1, threaded on the top-level mention
	// (100.000) and the mention buried in the reply (101.500) — never the
	// channel_join's ts, which would mean the mention filter let it through.
	// The third lands in D1, threaded on the DM message that named no
	// mention at all — the DM-doesn't-need-a-mention branch, scoped to D1
	// only (C1's unmentioned messages, 101.000 and the join, are absent).
	gotByChannel := map[string]map[string]bool{}
	for _, p := range *posted {
		ch, _ := p["channel"].(string)
		ts, _ := p["thread_ts"].(string)

		if gotByChannel[ch] == nil {
			gotByChannel[ch] = map[string]bool{}
		}

		gotByChannel[ch][ts] = true
	}

	if !gotByChannel["C1"]["100.000"] || !gotByChannel["C1"]["101.500"] || len(gotByChannel["C1"]) != 2 {
		t.Errorf("C1 thread_ts set = %v, want exactly {100.000, 101.500}", gotByChannel["C1"])
	}

	if !gotByChannel["D1"]["50.000"] || len(gotByChannel["D1"]) != 1 {
		t.Errorf("D1 thread_ts set = %v, want exactly {50.000}", gotByChannel["D1"])
	}
}

// TestEndToEndBuiltinSlackSurvivesRateLimits pins the retry on every call the
// two built-in types make, not only the fan-outs.
//
// Under steps watch this check runs on every poll interval, which makes
// auth.test and users.conversations — two serial calls before any fan-out —
// the most-hit endpoints here and the likeliest to be rate limited. Without a
// retry on them a single 429 fails the whole check ("slack auth.test:
// ratelimited") and loses that poll, while the identically-limited history
// fan-out immediately below would have backed off and succeeded. The same
// holds for chat.postMessage on the put side, where a 429 fails a step whose
// work is already done.
func TestEndToEndBuiltinSlackSurvivesRateLimits(t *testing.T) {
	server, posted := fakeSlackRateLimiting(t, map[string]int{
		"/api/auth.test":             1,
		"/api/users.conversations":   1,
		"/api/conversations.history": 1,
		"/api/chat.postMessage":      1,
	})
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resources:
- name: mentions
  type: slack-mentions
  source:
    base_url: ` + server.URL + `
- name: reply
  type: slack-reply
  source:
    base_url: ` + server.URL + `

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
      grep -o '"ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      printf '%s' "answered" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "answer")

	// The same three replies the un-rate-limited run produces: every 429 was
	// absorbed, none of them cost a message.
	if len(*posted) != 3 {
		t.Errorf("posted %d messages, want 3 — a rate limit cost work that a retry should have absorbed: %v", len(*posted), *posted)
	}
}

// TestEndToEndBuiltinSlackReplyWithoutThread proves the generalized branch:
// when thread/ts is absent (or empty), slack-reply posts a new top-level
// message instead of a threaded one — thread_ts must be omitted entirely,
// not sent empty, since Slack treats an empty thread_ts as an error.
func TestEndToEndBuiltinSlackReplyWithoutThread(t *testing.T) {
	server, posted := fakeSlack(t)
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-fake")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resources:
- name: reply
  type: slack-reply
  source:
    base_url: ` + server.URL + `

jobs:
- name: announce
  plan:
  - task: address
    outputs: [thread, answer]
    run: |
      set -eu
      printf '%s' "C1" > thread/channel
      printf '%s' "hello, world" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)
	mustRun(t, "run", path, "--job", "announce")

	if len(*posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(*posted))
	}

	if _, has := (*posted)[0]["thread_ts"]; has {
		t.Errorf("posted %v, want no thread_ts key for a top-level post", (*posted)[0])
	}

	if (*posted)[0]["channel"] != "C1" {
		t.Errorf("posted channel = %v, want C1", (*posted)[0]["channel"])
	}
}

// TestEndToEndBuiltinSlackReplyCustomTokenEnv proves the escape hatch for a
// second Slack bot in one pipeline: a resource that names its own env: entry
// and points source.token_env at it posts using THAT token's name, not
// SLACK_BOT_TOKEN — and never sees SLACK_BOT_TOKEN at all, since the type's
// default name isn't in this resource's widened allow-list.
func TestEndToEndBuiltinSlackReplyCustomTokenEnv(t *testing.T) {
	server, posted := fakeSlack(t)
	t.Setenv("SECOND_BOT_TOKEN", "xoxb-second")
	// Deliberately NOT set: SLACK_BOT_TOKEN. If the resource type's default
	// name leaked through, env() would fail with "unset", not "not allowed" —
	// either way this proves the custom name is what actually got used.

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resources:
- name: reply
  type: slack-reply
  env: [SECOND_BOT_TOKEN]
  source:
    base_url: ` + server.URL + `
    token_env: SECOND_BOT_TOKEN

jobs:
- name: announce
  plan:
  - task: address
    outputs: [thread, answer]
    run: |
      set -eu
      printf '%s' "C1" > thread/channel
      printf '%s' "hello from bot two" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)
	mustRun(t, "run", path, "--job", "announce")

	if len(*posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(*posted))
	}

	if (*posted)[0]["channel"] != "C1" {
		t.Errorf("posted channel = %v, want C1", (*posted)[0]["channel"])
	}
}

// TestEndToEndBuiltinSlackReplyUnlistedTokenEnvFails proves the allow-list
// still holds: naming a token_env without ALSO widening the resource's own
// env: doesn't grant it — env()'s sandbox rejects the name, it does not fall
// back to SLACK_BOT_TOKEN or succeed silently with an empty credential.
func TestEndToEndBuiltinSlackReplyUnlistedTokenEnvFails(t *testing.T) {
	server, _ := fakeSlack(t)
	t.Setenv("SECOND_BOT_TOKEN", "xoxb-second")

	dir := t.TempDir()
	path := filepath.Join(dir, "pipeline.yml")

	pipelineYAML := `
resources:
- name: reply
  type: slack-reply
  source:
    base_url: ` + server.URL + `
    token_env: SECOND_BOT_TOKEN

jobs:
- name: announce
  plan:
  - task: address
    outputs: [thread, answer]
    run: |
      set -eu
      printf '%s' "C1" > thread/channel
      printf '%s' "hello" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)

	err = run([]string{"run", path, "--job", "announce"})
	if err == nil {
		t.Fatal("run: want an error, SECOND_BOT_TOKEN is not in this resource's env:")
	}
}
