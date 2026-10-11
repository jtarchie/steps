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
