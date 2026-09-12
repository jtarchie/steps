package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// A delivery's check ends with its pipeline as well as its sender: a server's shutdown waits on a request rather than cancelling it, so a check still acquiring a machine outlived the daemon's Close and left the machine billing.
func TestWebhookCheckEndsWithItsPipeline(t *testing.T) {
	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	dir := t.TempDir()
	started := filepath.Join(dir, "started")

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "slow",
			Config: config.ResourceTypeConfig{Check: "touch " + started + "; exec sleep 30", In: "echo fetched"},
		}},
		Resources: []config.Resource{{Name: "repo", Type: "slow", WebhookTokenEnv: "STEPS_TEST_HOOK_TOKEN"}},
	}

	pipeline, destroy := context.WithCancel(context.Background())
	defer destroy()

	handler := &webhookHandler{current: staticConfig(cfg), st: mustOpenStore(t, dir), base: pipeline}
	answered := make(chan int, 1)

	go func() { answered <- post(t, handler, "/check/repo?token=s3cret") }()

	deadline := time.Now().Add(10 * time.Second)

	for {
		_, err := os.Stat(started)
		if err == nil {
			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the check never started")
		}

		time.Sleep(10 * time.Millisecond)
	}

	destroy()

	select {
	case code := <-answered:
		if code != http.StatusInternalServerError {
			t.Errorf("status = %d, want the check its pipeline ended reported as failed", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the check outlived its pipeline")
	}
}

// webhookFixture builds a pipeline with one webhook-enabled resource and the
// handler that serves it.
func webhookFixture(t *testing.T) *webhookHandler {
	t.Helper()

	dir := t.TempDir()
	versions := dir + "/versions.json"

	writeVersions(t, versions, `[{"ref":"v1"}]`)

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "listing",
			Config: config.ResourceTypeConfig{Check: "cat " + versions, In: "echo fetched"},
		}},
		Resources: []config.Resource{{
			Name: "repo", Type: "listing", WebhookTokenEnv: "STEPS_TEST_HOOK_TOKEN",
		}},
		Jobs: []config.Job{{
			Name: "build",
			Plan: []config.Step{
				{Get: "repo", Trigger: true},
				{Task: "work", Run: "true", Inputs: config.Inputs()},
			},
		}},
	}

	st := mustOpenStore(t, dir)

	return &webhookHandler{current: staticConfig(cfg), st: st, base: context.Background()}
}

// post issues a webhook request and returns the status code.
func post(t *testing.T, handler http.Handler, target string) int {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, target, nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	return rec.Code
}

// TestWebhookTriggersAnImmediateCheck is the feature: an outside system says
// "check now" and the affected job is queued, rather than waiting up to a full
// poll interval.
func TestWebhookTriggersAnImmediateCheck(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	if code := post(t, handler, "/check/repo?token=s3cret"); code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}

	_, name, found, err := handler.st.ClaimNextJob(context.Background())
	if err != nil {
		t.Fatalf("ClaimNextJob: %v", err)
	}

	if !found || name != "build" {
		t.Errorf("claimed %q (found=%v), want the affected job queued immediately", name, found)
	}
}

// A delivery that enqueues without recording what it checked leaves the next poll a cold start, which builds the same version a second time.
func TestWebhookRecordsTheVersionItChecked(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	deliverThenPollQuietly(t, handler, "v1", 1)

	// Past the cold start, where seeding no longer records the checked version on the delivery's behalf.
	writeVersions(t, handler.current().ResourceTypes[0].Config.Check[len("cat "):], `[{"ref":"v1"},{"ref":"v2"}]`)

	deliverThenPollQuietly(t, handler, "v2", 2)
}

// History is asserted before the poll because the poll files whatever the delivery failed to.
func deliverThenPollQuietly(t *testing.T, handler *webhookHandler, wantRef string, wantHistory int) {
	t.Helper()

	if code := post(t, handler, "/check/repo?token=s3cret"); code != http.StatusOK {
		t.Fatalf("delivering %s: status = %d, want 200", wantRef, code)
	}

	ctx := context.Background()

	_, version, found, err := recordedVersion(ctx, handler.st, "repo")
	if err != nil {
		t.Fatalf("recordedVersion: %v", err)
	}

	if !found || version["ref"] != wantRef {
		t.Fatalf("recorded %v (found=%v), want %s, the version the delivery checked", version, found, wantRef)
	}

	history, err := handler.st.ResourceVersionsJSON(ctx, "repo")
	if err != nil {
		t.Fatalf("ResourceVersionsJSON: %v", err)
	}

	if len(history) != wantHistory {
		t.Errorf("history = %v, want %s filed where the build it enqueued resolves versions", history, wantRef)
	}

	enqueued, err := pollOnce(ctx, handler.current(), handler.st)
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}

	if len(enqueued) != 0 {
		t.Errorf("the poll after delivering %s enqueued %v, want nothing — the delivery already built it", wantRef, enqueued)
	}
}

type probeKey struct{}

// A request's context carries none of the daemon's values, so what it cannot answer must come from the daemon's.
func TestRequestContextFallsBackToTheDaemonsValues(t *testing.T) {
	t.Parallel()

	base := context.WithValue(context.Background(), probeKey{}, "daemon")

	if got := (requestContext{Context: context.Background(), base: base}).Value(probeKey{}); got != "daemon" {
		t.Errorf("value = %v, want the daemon's", got)
	}

	request := context.WithValue(context.Background(), probeKey{}, "request")

	if got := (requestContext{Context: request, base: base}).Value(probeKey{}); got != "request" {
		t.Errorf("value = %v, want the request's own", got)
	}

	if got := (requestContext{Context: context.Background()}).Value(probeKey{}); got != nil {
		t.Errorf("value = %v, want nothing when neither context has it", got)
	}
}

// TestWebhookRejectsABadToken covers the obvious one.
func TestWebhookRejectsABadToken(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	if code := post(t, handler, "/check/repo?token=wrong"); code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}

// TestWebhookRejectsAnUnsetToken covers the misconfiguration that would
// otherwise be worse than a wrong token: an empty expectation read as "no auth
// required" turns a deployment mistake into an open trigger endpoint.
func TestWebhookRejectsAnUnsetToken(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "")

	for _, target := range []string{"/check/repo?token=", "/check/repo?token=anything"} {
		if code := post(t, handler, target); code != http.StatusUnauthorized {
			t.Errorf("POST %s: status = %d, want 401 when the token variable is unset", target, code)
		}
	}
}

// TestWebhookDoesNotLeakResourceNames verifies an unknown resource and a bad
// token are indistinguishable. Otherwise the endpoint is a free directory of a
// pipeline's resource names to anyone who can reach it.
func TestWebhookDoesNotLeakResourceNames(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	known := post(t, handler, "/check/repo?token=wrong")
	unknown := post(t, handler, "/check/does-not-exist?token=wrong")

	if known != unknown {
		t.Errorf("a wrong token gives %d for a known resource and %d for an unknown one; the difference is a directory listing", known, unknown)
	}
}

// TestWebhookRejectsGet keeps the endpoint off anything that follows links: a
// browser preview or a link scanner must not be able to start a pipeline.
func TestWebhookRejectsGet(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/check/repo?token=s3cret", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestWebhookAcceptsABearerToken covers the header form, which is what a
// sender that will not put a secret in a URL uses.
func TestWebhookAcceptsABearerToken(t *testing.T) {
	handler := webhookFixture(t)

	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/check/repo", nil)
	req.Header.Set("Authorization", "Bearer s3cret")

	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a bearer token", rec.Code)
	}
}

// TestWebhookFollowsAConfigSwap is the seam between the daemon's reload and
// this endpoint. The handler used to capture the configuration it was mounted
// with, which made two things impossible to fix without a restart: a pipeline
// that GAINED its first webhook_token_env: was mounted as nil and 404'd
// forever, and one that LOST a resource kept an endpoint live that
// authenticated against a token the operator believed they had deleted.
func TestWebhookFollowsAConfigSwap(t *testing.T) {
	t.Setenv("STEPS_TEST_HOOK_TOKEN", "s3cret")

	fixture := webhookFixture(t)
	served := fixture.current()

	current := &atomic.Pointer[config.Config]{}
	current.Store(served)

	handler := &webhookHandler{current: current.Load, st: fixture.st}

	if code := post(t, handler, "/check/repo?token=s3cret"); code != http.StatusOK {
		t.Fatalf("before the swap: status = %d, want 200", code)
	}

	// The edit that revokes it: the resource keeps its name and loses its
	// webhook_token_env:, which is how an operator turns the endpoint off.
	revoked := *served
	revoked.Resources = []config.Resource{{Name: "repo", Type: "listing"}}
	current.Store(&revoked)

	if code := post(t, handler, "/check/repo?token=s3cret"); code == http.StatusOK {
		t.Error("a revoked webhook token still triggered a check after the swap")
	}
}
