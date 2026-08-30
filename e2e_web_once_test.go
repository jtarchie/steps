package main

// `steps web --once` and the webhook route: the two things `steps web` did
// that the daemon had to grow before that command could go.
//
// --once is the cron form — poll, drain, exit — and its distinguishing
// property is what it does NOT do: bind the listen address. A one-shot that
// left a listener behind would be a daemon with extra steps, and a port
// opened for the duration of one poll is a port nothing has time to reach.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// TestWebOnceNeverBinds is the property that makes --once usable from cron.
//
// It is asserted by HOLDING the address the command would bind: a --once that
// tried to serve would fail to listen, and this test would see the error
// instead of the work.
func TestWebOnceNeverBinds(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	var config net.ListenConfig

	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = listener.Close() }()

	err = run([]string{"web", fixture.pipeline, "--once", "--listen", listener.Addr().String()})
	if err != nil {
		t.Fatalf("web --once: %v", err)
	}

	// It polled and it ran: the point of a one-shot is that it does the work,
	// not merely that it exits.
	if did := fixture.did(t); strings.Join(did, " ") != "1" {
		t.Errorf("processed %v, want the one version the feed held", did)
	}
}

// TestWebOnceDrainsWhatItEnqueued: one poll can enqueue several jobs, so
// "once" means one POLL rather than one build. It has to keep draining until
// the queue is empty, or a cron-driven steps quietly falls behind by however
// many versions arrived between ticks.
func TestWebOnceDrainsWhatItEnqueued(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 3)

	// The first poll is a cold start, which answers the newest version and
	// records the rest as taken — see docs/resources.md. That is what makes
	// the SECOND poll the interesting one.
	err := run([]string{"web", fixture.pipeline, "--once"})
	if err != nil {
		t.Fatalf("web --once: %v", err)
	}

	fixture.items(t, 5)

	err = run([]string{"web", fixture.pipeline, "--once"})
	if err != nil {
		t.Fatalf("web --once: %v", err)
	}

	// version: every, so both arrivals are separate builds off one poll.
	if did := fixture.did(t); strings.Join(did, " ") != "3 4 5" {
		t.Errorf("processed %v, want the cold start plus both versions the second poll found", did)
	}
}

// webhookPipeline is a resource with a webhook token, plus the job a check of
// it triggers.
const webhookPipeline = `
defaults:
  preflight:
    disabled: true
resource_types:
- name: feed
  config:
    check: |
      awk 'BEGIN{printf "["} {printf "%s{\"n\":\"%s\"}", (k++?",":""), $1} END{printf "]"}' FEED
    in: echo {{ .version.n | shellquote }} > n.txt
resources:
- name: items
  type: feed
  source: {}
  webhook_token_env: STEPS_TEST_WEBHOOK_TOKEN
jobs:
- name: build
  plan:
  - get: items
    trigger: true
  - task: work
    inputs: [items]
    run: cat items/n.txt >> PROCESSED
`

// TestWebhookRouteTriggersACheck.
//
// `steps watch --listen` (before the daemons merged) served this on a second port of its own, so a
// deployment that wanted the UI and webhooks had two addresses to expose. One
// daemon means one listener, and the route sits under the pipeline it checks
// because a pipeline-blind path is ambiguous the moment a process serves two.
func TestWebhookRouteTriggersACheck(t *testing.T) {
	t.Setenv("STEPS_TEST_WEBHOOK_TOKEN", "s3cret")

	fixture := newWatchFixture(t, webhookPipeline)
	fixture.items(t, 1)

	// A long interval, so nothing the poll loop does can be mistaken for the
	// webhook having worked.
	served := startWeb(t, []string{fixture.pipeline}, "--interval", "1h")
	defer served.stop(t)

	slug := pipelineName(fixture.pipeline)
	url := fmt.Sprintf("http://%s/p/%s/check/items?token=s3cret", served.addr, slug)

	if status := postWebhook(t, url); status != http.StatusOK {
		t.Fatalf("webhook answered %d, want 200", status)
	}

	waitForDid(t, fixture, "1")
}

// TestWebhookRouteRefusesABadToken: the token is the whole authentication of
// this route, which is also why it is exempt from the same-origin check every
// browser mutation gets.
func TestWebhookRouteRefusesABadToken(t *testing.T) {
	t.Setenv("STEPS_TEST_WEBHOOK_TOKEN", "s3cret")

	fixture := newWatchFixture(t, webhookPipeline)
	fixture.items(t, 1)

	served := startWeb(t, []string{fixture.pipeline}, "--interval", "1h")
	defer served.stop(t)

	slug := pipelineName(fixture.pipeline)
	url := fmt.Sprintf("http://%s/p/%s/check/items?token=wrong", served.addr, slug)

	if status := postWebhook(t, url); status != http.StatusUnauthorized {
		t.Errorf("webhook answered %d for a bad token, want 401", status)
	}
}

// TestWebhookRouteIsAbsentWithoutWebhookResources: a pipeline that names no
// webhook_token_env: has nothing to trigger, so the honest answer is 404 —
// not a published endpoint that authenticates nothing.
func TestWebhookRouteIsAbsentWithoutWebhookResources(t *testing.T) {
	fixture := newWatchFixture(t, cursorFeed)
	fixture.items(t, 1)

	served := startWeb(t, []string{fixture.pipeline}, "--interval", "1h")
	defer served.stop(t)

	slug := pipelineName(fixture.pipeline)
	url := fmt.Sprintf("http://%s/p/%s/check/items?token=anything", served.addr, slug)

	if status := postWebhook(t, url); status != http.StatusNotFound {
		t.Errorf("webhook answered %d, want 404 for a pipeline with no webhook resources", status)
	}
}

// postWebhook sends the POST a webhook sender would and returns its status.
//
// Keep-alives off and the body drained, both deliberately: a connection left
// pooled is one http.Server.Shutdown waits the full grace period for, which
// turns every test here into a five-second stop and reports as a shutdown
// failure rather than as the assertion that was actually being made.
func postWebhook(t *testing.T, url string) int {
	t.Helper()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode
}
