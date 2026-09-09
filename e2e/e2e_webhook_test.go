package e2e

// The webhook route: an outside system saying "check this resource now", on the one address the daemon serves.

import (
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

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
	served := startWebFor(t, fixture.pipeline, "--interval", "1h")
	defer served.stop(t)

	slug := cli.PipelineName(fixture.pipeline)
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

	served := startWebFor(t, fixture.pipeline, "--interval", "1h")
	defer served.stop(t)

	slug := cli.PipelineName(fixture.pipeline)
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

	served := startWebFor(t, fixture.pipeline, "--interval", "1h")
	defer served.stop(t)

	slug := cli.PipelineName(fixture.pipeline)
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

// TestWebhookRouteUnderAPipelineNamedCheck.
//
// The route is /p/<slug>/check/<resource>, and the handler finds the resource
// by looking for "/check/" in the path. Looking from the FRONT made a pipeline
// whose slug is literally `check` produce /p/check/check/<resource>, whose
// first match yields "check/<resource>" — a name with a slash in it, rejected
// as not-found before the token is even read. A correctly-signed delivery got
// a permanent 404 and steps logged nothing, so the only symptom was a webhook
// that never fired for one arbitrary-looking pipeline name.
//
// This is the seam test: the handler is exercised unmounted elsewhere and the
// route is exercised with a safe slug elsewhere; only the two together find
// this.
func TestWebhookRouteUnderAPipelineNamedCheck(t *testing.T) {
	t.Setenv("STEPS_TEST_WEBHOOK_TOKEN", "s3cret")

	fixture := newWatchFixtureIn(t, t.TempDir(), "check", webhookPipeline)
	fixture.items(t, 1)

	served := startWebFor(t, fixture.pipeline, "--interval", "1h")
	defer served.stop(t)

	slug := cli.PipelineName(fixture.pipeline)
	if slug != "check" {
		t.Fatalf("slug = %q, want check — this test proves nothing otherwise", slug)
	}

	url := fmt.Sprintf("http://%s/p/%s/check/items?token=s3cret", served.addr, slug)

	if status := postWebhook(t, url); status != http.StatusOK {
		t.Fatalf("webhook answered %d for a pipeline named check, want 200", status)
	}

	waitForDid(t, fixture, "1")
}

// TestWebhookRouteStillWorksUnderReadOnly pins a decision, not an accident.
//
// Every other mutating route refuses when --read-only withholds the runner;
// this one does not, and the difference is deliberate: it authenticates with
// the resource's own token rather than riding the server's absent
// authentication, and a read-only build box that could not be notified is most
// of what a read-only build box is for. Without this test the behavior is
// indistinguishable from the missing `s.runner == nil` guard its four siblings
// all have — which is how a hole and a design decision look identical.
//
// docs/web.md and docs/infra.md both say so now. If that call is ever
// reversed, this test is what has to change with it.
func TestWebhookRouteStillWorksUnderReadOnly(t *testing.T) {
	t.Setenv("STEPS_TEST_WEBHOOK_TOKEN", "s3cret")

	fixture := newWatchFixture(t, webhookPipeline)
	fixture.items(t, 1)

	served := startWebFor(t, fixture.pipeline, "--read-only", "--interval", "1h")
	defer served.stop(t)

	slug := cli.PipelineName(fixture.pipeline)

	// The browser control is withheld, which is what --read-only means.
	trigger := fmt.Sprintf("http://%s/p/%s/jobs/build/trigger", served.addr, slug)
	if status := postWebhook(t, trigger); status != http.StatusForbidden {
		t.Errorf("the UI trigger answered %d under --read-only, want 403", status)
	}

	// The token-authenticated one is not — and the job it enqueues runs.
	hook := fmt.Sprintf("http://%s/p/%s/check/items?token=s3cret", served.addr, slug)
	if status := postWebhook(t, hook); status != http.StatusOK {
		t.Fatalf("webhook answered %d under --read-only, want 200", status)
	}

	waitForDid(t, fixture, "1")

	// And a bad token is still refused, on a read-only server as anywhere.
	bad := fmt.Sprintf("http://%s/p/%s/check/items?token=wrong", served.addr, slug)
	if status := postWebhook(t, bad); status != http.StatusUnauthorized {
		t.Errorf("webhook answered %d for a bad token under --read-only, want 401", status)
	}
}
