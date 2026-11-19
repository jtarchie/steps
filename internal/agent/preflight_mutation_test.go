package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// TestAProbeSaysWhichWayItFailed: a probe that ran out of time says so, and one the provider refused carries the provider's own answer — reading a refusal as a timeout sends the operator off raising retries.
func TestAProbeSaysWhichWayItFailed(t *testing.T) {
	t.Parallel()

	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	t.Cleanup(hung.Close)

	err := probeModel(context.Background(), config.ResolvedInvocation{BaseURL: hung.URL + "/v1/", ModelName: "m"}, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "no response within 50ms") {
		t.Errorf("hung provider: err = %v; want it named a timeout", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(refusing.Close)

	err = probeModel(context.Background(), config.ResolvedInvocation{BaseURL: refusing.URL + "/v1/", ModelName: "m"}, 10*time.Second)
	if err == nil || strings.Contains(err.Error(), "no response within") || !strings.Contains(err.Error(), "400") {
		t.Errorf("refusing provider: err = %v; want the provider's own answer", err)
	}
}

// TestAProbeRefusalCarriesTheProvidersReason: OpenRouter answers an exhausted key with a bare 403, and the reason is only in the body; the probe sent nothing but "hi", so its message can echo nothing private.
func TestAProbeRefusalCarriesTheProvidersReason(t *testing.T) {
	t.Parallel()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Key limit exceeded (total limit)"}}`))
	}))
	t.Cleanup(refusing.Close)

	err := probeModel(context.Background(), config.ResolvedInvocation{BaseURL: refusing.URL + "/v1/", ModelName: "m"}, 10*time.Second)
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Key limit exceeded (total limit)") {
		t.Errorf("err = %v; want the status and the provider's reason", err)
	}
}

// The quoted reason is stored and shown to every viewer, so the key a provider echoes back and bytes a terminal would act on stay out of it.
func TestAProbeRefusalQuotesNoKeyAndNoControlBytes(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":401,"message":"key sk-or-v1-abc123 is \u001b[31mrevoked"}}`))
	}))
	t.Cleanup(refusing.Close)

	t.Setenv("PROBE_TEST_KEY", "sk-or-v1-abc123")

	err := probeModel(context.Background(), config.ResolvedInvocation{BaseURL: refusing.URL + "/v1/", ModelName: "m", APIKeyEnv: "PROBE_TEST_KEY"}, 10*time.Second) //nolint:gosec // the name of a variable, not a credential
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("err = %v; want the provider's reason", err)
	}

	if strings.Contains(err.Error(), "abc123") || strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("err = %q; want the key masked and the escape dropped", err)
	}
}
