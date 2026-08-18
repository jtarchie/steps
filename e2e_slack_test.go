package main

import (
	"cmp"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
)

// slackMsg is one message in a fake workspace's transcript. parent is empty
// for a top-level message and the parent's ts for a thread reply — the
// distinction conversations.history and conversations.replies are built
// around, and the one the check has to navigate.
type slackMsg struct {
	channel string
	ts      string
	user    string
	text    string
	subtype string
	parent  string
}

// fakeSlackTranscript is the default workspace both built-in types are tested
// against, shaped to exercise every branch a real check has to get right: a
// top-level message that mentions the bot, an unrelated top-level message
// whose only relevance is that it has a reply, and a reply — buried in that
// thread — that mentions the bot. A channel_join system message also contains
// the mention text ("<@UBOT> has joined"), which must NOT count.
//
// D1 is a 1:1 DM (Slack's own "D" id prefix): its one message names no
// mention at all, and must still be picked up — the DM-doesn't-need-a-mention
// branch. C1 stays mention-gated throughout, so a DM-shaped message without a
// mention landing there would prove the branch leaked past its channel.
func fakeSlackTranscript() []slackMsg {
	return []slackMsg{
		{channel: "C1", ts: "100.000", user: "U2", text: "hey <@UBOT> help?"},
		{channel: "C1", ts: "101.000", user: "U2", text: "no mention here"},
		{channel: "C1", ts: "101.500", user: "U3", text: "actually <@UBOT> can you look?", parent: "101.000"},
		{channel: "C1", ts: "102.000", user: "U2", text: "<@UBOT> has joined the channel", subtype: "channel_join"},
		{channel: "D1", ts: "50.000", user: "U2", text: "no bot mention needed, this is a DM"},
	}
}

// fakeSlackWorkspace is a fake Slack web API: auth.test resolves the bot's own
// id, conversations.history/replies answer from a transcript a test can append
// to mid-run, and chat.postMessage records what was posted.
//
// The two read endpoints follow the real API's shape, since that shape is what
// the check has to work with: history returns TOP-LEVEL messages only, newest
// first, annotating a thread parent with reply_count and latest_reply;
// oldest is an EXCLUSIVE floor on both endpoints; limit truncates the OLD end
// of history; and conversations.replies always returns its parent message
// first, even when the parent is older than oldest. All four are verified
// against the live API.
type fakeSlackWorkspace struct {
	t            *testing.T
	mu           sync.Mutex
	transcript   []slackMsg
	posted       []map[string]any
	rateLimited  map[string]int
	repliesAsked []string
}

// fakeSlack serves the default transcript — all most tests need. Read what it
// posted through postedMessages(), never the field: the handler runs on the
// server's own goroutine and every accessor here takes the mutex for that
// reason.
func fakeSlack(t *testing.T) (*httptest.Server, *fakeSlackWorkspace) {
	t.Helper()

	return fakeSlackServing(t, nil)
}

// fakeSlackRateLimiting is fakeSlack with a budget of 429s to serve first, per
// API path — how a real workspace answers when a watch host polls it faster
// than its rate limit allows.
func fakeSlackRateLimiting(t *testing.T, rateLimited map[string]int) (*httptest.Server, *fakeSlackWorkspace) {
	t.Helper()

	return fakeSlackServing(t, rateLimited)
}

// fakeSlackServing builds the workspace both constructors above wrap.
func fakeSlackServing(t *testing.T, rateLimited map[string]int) (*httptest.Server, *fakeSlackWorkspace) {
	t.Helper()

	workspace := &fakeSlackWorkspace{t: t, transcript: fakeSlackTranscript(), rateLimited: rateLimited}

	server := httptest.NewServer(http.HandlerFunc(workspace.serve))
	t.Cleanup(server.Close)

	return server, workspace
}

// say appends a message to the transcript, the way a person talking in Slack
// between two polls does.
func (f *fakeSlackWorkspace) say(msg slackMsg) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.transcript = append(f.transcript, msg)
}

// threadsAsked is every thread the CHECK called conversations.replies for, as
// "channel/parent-ts" — what discovering thread mentions costs. A get's own
// fetch of the thread it is delivering carries no oldest and is not part of
// that cost, so it is not counted here.
func (f *fakeSlackWorkspace) threadsAsked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.repliesAsked)
}

// postedMessages is what chat.postMessage has been asked to send, so far.
func (f *fakeSlackWorkspace) postedMessages() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.posted)
}

func (f *fakeSlackWorkspace) serve(w http.ResponseWriter, r *http.Request) {
	if f.rateLimit(r.URL.Path) {
		// Retry-After: 0 so the test does not actually wait out a backoff.
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	query := r.URL.Query()

	switch r.URL.Path {
	case "/api/auth.test":
		f.answer(w, map[string]any{"ok": true, "user_id": "UBOT"})
	case "/api/users.conversations":
		f.answer(w, map[string]any{
			"ok":       true,
			"channels": []map[string]string{{"id": "C1"}, {"id": "D1"}},
		})
	case "/api/conversations.history":
		f.answer(w, map[string]any{
			"ok":       true,
			"messages": f.history(query.Get("channel"), query.Get("oldest"), query.Get("limit")),
		})
	case "/api/conversations.replies":
		f.answer(w, map[string]any{
			"ok":       true,
			"messages": f.replies(query.Get("channel"), query.Get("ts"), query.Get("oldest")),
		})
	case "/api/chat.postMessage":
		f.post(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		f.answer(w, map[string]any{"ok": false, "error": "unknown_method"})
	}
}

// answer writes one Slack JSON response. A payload that cannot be encoded is
// a broken fixture, not something a check should be asked to interpret.
func (f *fakeSlackWorkspace) answer(w http.ResponseWriter, payload map[string]any) {
	err := json.NewEncoder(w).Encode(payload)
	if err != nil {
		f.t.Errorf("encoding a fake Slack response: %v", err)
	}
}

// rateLimit spends one of this path's budgeted 429s, reporting whether the
// caller should serve one instead of an answer.
func (f *fakeSlackWorkspace) rateLimit(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	remaining := f.rateLimited[path]
	if remaining <= 0 {
		return false
	}

	f.rateLimited[path] = remaining - 1

	return true
}

// history answers conversations.history: a channel's top-level messages,
// newest first, each thread parent carrying reply_count and latest_reply.
// oldest is an exclusive floor and limit truncates the oldest end — so a
// caller widening its window can only ever lose the far end of history, never
// the recent messages it is actually polling for.
func (f *fakeSlackWorkspace) history(channel, oldest, limit string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	floor := slackTS(oldest)

	// Never nil: Slack answers an empty range with [], and a check written
	// against a null it never sends would be tested against a fiction.
	messages := []map[string]any{}

	for _, msg := range f.transcript {
		if msg.channel != channel || msg.parent != "" || slackTS(msg.ts) <= floor {
			continue
		}

		messages = append(messages, f.annotate(msg))
	}

	slices.SortFunc(messages, func(a, b map[string]any) int {
		left, _ := a["ts"].(string)
		right, _ := b["ts"].(string)

		return cmp.Compare(slackTS(right), slackTS(left))
	})

	count, err := strconv.Atoi(limit)
	if err == nil && count > 0 && len(messages) > count {
		messages = messages[:count]
	}

	return messages
}

// annotate renders one message as Slack's JSON, adding the thread fields a
// parent carries. Callers hold f.mu.
func (f *fakeSlackWorkspace) annotate(msg slackMsg) map[string]any {
	rendered := map[string]any{"type": "message", "user": msg.user, "ts": msg.ts, "text": msg.text}
	if msg.subtype != "" {
		rendered["subtype"] = msg.subtype
	}

	replies := 0
	latest := ""

	for _, other := range f.transcript {
		if other.channel != msg.channel || other.parent != msg.ts {
			continue
		}

		replies++

		if slackTS(other.ts) > slackTS(latest) {
			latest = other.ts
		}
	}

	if replies > 0 {
		rendered["thread_ts"] = msg.ts
		rendered["reply_count"] = replies
		rendered["latest_reply"] = latest
	}

	return rendered
}

// replies answers conversations.replies: the parent message first — always,
// even when it is older than oldest — then every reply above that exclusive
// floor.
func (f *fakeSlackWorkspace) replies(channel, parent, oldest string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	if oldest != "" {
		f.repliesAsked = append(f.repliesAsked, channel+"/"+parent)
	}

	floor := slackTS(oldest)

	messages := []map[string]any{}

	for _, msg := range f.transcript {
		if msg.channel != channel {
			continue
		}

		if msg.ts == parent {
			messages = append(messages, f.annotate(msg))

			continue
		}

		if msg.parent == parent && slackTS(msg.ts) > floor {
			messages = append(messages, f.annotate(msg))
		}
	}

	return messages
}

func (f *fakeSlackWorkspace) post(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	var payload map[string]any

	_ = json.Unmarshal(body, &payload)

	f.mu.Lock()
	f.posted = append(f.posted, payload)
	f.mu.Unlock()

	f.answer(w, map[string]any{"ok": true, "channel": payload["channel"], "ts": "999.999"})
}

// slackTS parses a Slack ts for comparison. A ts is a string and must be
// compared as a number — "1000.0" sorts before "999.0" as text.
func slackTS(ts string) float64 {
	parsed, err := strconv.ParseFloat(ts, 64)
	if err != nil {
		return 0
	}

	return parsed
}

// TestEndToEndBuiltinSlackMentionsAndReply drives both built-in types through
// the whole CLI stack against a fake Slack API: check finds both mentions (one
// top-level, one buried in a thread the parent message doesn't itself
// mention), version: every fans the job out once per mention, and each fetched
// thread is answered in place — proving the two types compose the way the
// hand-written pipeline they were extracted from does.
//
// The reply text carries the message count of the thread the get delivered,
// because delivering a thread is the point: a mention that arrives as a reply
// has to bring the conversation it was written inside, not just the sentence
// that named the bot. An answer written without the question is worse than no
// answer, and the only visible difference is this count.
func TestEndToEndBuiltinSlackMentionsAndReply(t *testing.T) {
	server, workspace := fakeSlack(t)
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
      grep -o '"thread_ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      printf 'read %s messages' "$(grep -c '"ts":' mentions/thread.json)" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "validate", path)
	mustRun(t, "run", path, "--job", "answer")

	posted := workspace.postedMessages()
	if len(posted) != 3 {
		t.Fatalf("posted %d messages, want 3 (two channel mentions + one unmentioned DM): %v", len(posted), posted)
	}

	// Two replies land in channel C1: one on the top-level mention (100.000)
	// and one on the PARENT of the mention buried in a thread (101.000, not
	// the 101.500 reply itself — Slack's own guidance is to thread on the
	// parent, and a reply's ts is not a thread id). Never the channel_join's
	// ts, which would mean the mention filter let it through. The third lands
	// in D1, threaded on the DM message that named no mention at all — the
	// DM-doesn't-need-a-mention branch, scoped to D1 only (C1's unmentioned
	// messages, 101.000 and the join, are absent).
	//
	// The thread delivered with the 101.500 mention has both messages in it;
	// the two that are their own parents have one.
	got := map[string]string{}
	for _, p := range posted {
		channel, _ := p["channel"].(string)
		ts, _ := p["thread_ts"].(string)
		text, _ := p["text"].(string)

		got[channel+"/"+ts] = text
	}

	want := map[string]string{
		"C1/100.000": "read 1 messages",
		"C1/101.000": "read 2 messages",
		"D1/50.000":  "read 1 messages",
	}

	for key, wantText := range want {
		if got[key] != wantText {
			t.Errorf("posted to %s = %q, want %q", key, got[key], wantText)
		}
	}

	if len(got) != len(want) {
		t.Errorf("posted to %v, want exactly %v", got, want)
	}
}

// TestEndToEndBuiltinSlackAnswersAReplyBehindTheCursor is the failure this
// bot was actually reported for: a person replies to a thread whose parent was
// posted hours ago, mentions the bot, and nothing answers.
//
// The cursor is a single ts, and a thread reply does not move its parent's ts —
// only latest_reply. So a check that asks conversations.history for
// `oldest=cursor` stops being able to SEE the parent the moment anything
// newer, in any channel, moves the cursor past it — and conversations.replies
// is only ever asked about parents that history returned. The window that
// discovers threads therefore cannot be the cursor: it is `limit` messages of
// channel history, and the cursor decides what is NEW inside it.
//
// Two polls, because one cannot express it: the first is the cold start that
// takes the backlog without answering it, and only then is the cursor ahead of
// the parent.
func TestEndToEndBuiltinSlackAnswersAReplyBehindTheCursor(t *testing.T) {
	server, workspace := fakeSlackServing(t, nil)
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
      grep -o '"thread_ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
      printf '%s' "answered" > answer/reply.md
  - put: reply
    inputs: [thread, answer]
`

	err := os.WriteFile(path, []byte(pipelineYAML), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "watch", path, "--once")

	if got := workspace.postedMessages(); len(got) != 0 {
		t.Fatalf("cold start posted %d messages, want 0 — a fresh watcher records the backlog, it does not answer it: %v", len(got), got)
	}

	// The cursor now sits at 101.500, the newest mention the cold start
	// recorded. 100.000 — the parent replied to below — is behind it.
	workspace.say(slackMsg{channel: "C1", ts: "300.000", user: "U3", text: "<@UBOT> still there?", parent: "100.000"})

	before := len(workspace.threadsAsked())

	mustRun(t, "watch", path, "--once")

	// The wider discovery window is not a wider fan-out: a thread whose
	// latest_reply is behind the cursor has nothing new in it and must cost
	// no call, however many polls run. Without that gate this check would
	// read every thread in `limit` messages of history, every poll — the
	// reason the window was tied to the cursor in the first place.
	for _, asked := range workspace.threadsAsked()[before:] {
		if asked != "C1/100.000" {
			t.Errorf("fetched thread %s, want only C1/100.000 — the one with a reply newer than the cursor", asked)
		}
	}

	posted := workspace.postedMessages()
	if len(posted) != 1 {
		t.Fatalf("posted %d messages, want exactly 1 (the new reply, and nothing the cold start already took): %v", len(posted), posted)
	}

	// Threaded on the PARENT, 100.000 — the answer belongs in the
	// conversation the question was asked in, and 300.000 is a reply, not a
	// thread.
	if posted[0]["channel"] != "C1" || posted[0]["thread_ts"] != "100.000" {
		t.Errorf("posted %v, want a reply threaded on C1/100.000", posted[0])
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
	server, workspace := fakeSlackRateLimiting(t, map[string]int{
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
      grep -o '"thread_ts": *"[^"]*"' mentions/version.json | cut -d'"' -f4 > thread/ts
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
	posted := workspace.postedMessages()
	if len(posted) != 3 {
		t.Errorf("posted %d messages, want 3 — a rate limit cost work that a retry should have absorbed: %v", len(posted), posted)
	}
}

// TestEndToEndBuiltinSlackReplyWithoutThread proves the generalized branch:
// when thread/ts is absent (or empty), slack-reply posts a new top-level
// message instead of a threaded one — thread_ts must be omitted entirely,
// not sent empty, since Slack treats an empty thread_ts as an error.
func TestEndToEndBuiltinSlackReplyWithoutThread(t *testing.T) {
	server, workspace := fakeSlack(t)
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

	posted := workspace.postedMessages()
	if len(posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(posted))
	}

	if _, has := posted[0]["thread_ts"]; has {
		t.Errorf("posted %v, want no thread_ts key for a top-level post", posted[0])
	}

	if posted[0]["channel"] != "C1" {
		t.Errorf("posted channel = %v, want C1", posted[0]["channel"])
	}
}

// TestEndToEndBuiltinSlackReplyCustomTokenEnv proves the escape hatch for a
// second Slack bot in one pipeline: a resource that names its own env: entry
// and points source.token_env at it posts using THAT token's name, not
// SLACK_BOT_TOKEN — and never sees SLACK_BOT_TOKEN at all, since the type's
// default name isn't in this resource's widened allow-list.
func TestEndToEndBuiltinSlackReplyCustomTokenEnv(t *testing.T) {
	server, workspace := fakeSlack(t)
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

	posted := workspace.postedMessages()
	if len(posted) != 1 {
		t.Fatalf("posted %d messages, want 1", len(posted))
	}

	if posted[0]["channel"] != "C1" {
		t.Errorf("posted channel = %v, want C1", posted[0]["channel"])
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
