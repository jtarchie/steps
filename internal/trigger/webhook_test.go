package trigger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

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
