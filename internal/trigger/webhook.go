package trigger

// Webhook-triggered checks: let an outside system say "check this resource
// now" instead of waiting for the next poll.
//
// Polling already works, so this is a latency and rate-limit optimization
// rather than a correctness gap: a short interval reacts fast but burns API
// calls, a long one reacts slowly, and a webhook removes the tradeoff — react
// instantly, poll rarely as a safety net.
//
// The poll loop deliberately keeps running alongside it. A webhook that is
// missed (a delivery failure, a restart mid-flight) must not mean a change is
// never noticed.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// webhookHandler serves POST /check/<resource>?token=… for the resources a
// pipeline has given a webhook_token_env:.
type webhookHandler struct {
	cfg    *config.Config
	st     *store.Store
	tokens map[string]string
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// A GET would be triggerable from a browser preview or a link
		// scanner, which is not a thing a pipeline should be startable by.
		http.Error(w, "use POST", http.StatusMethodNotAllowed)

		return
	}

	// The last segment of .../check/<resource>. Taken from the end rather
	// than by trimming a fixed prefix because this handler is mounted BY its
	// server, and the web UI mounts it under the pipeline it belongs to
	// (/p/<slug>/check/<resource>) — a pipeline-blind route would be
	// ambiguous the moment one process serves two pipelines.
	_, name, found := strings.Cut(r.URL.Path, "/check/")
	if !found || name == "" || strings.Contains(name, "/") {
		http.Error(w, "not found", http.StatusNotFound)

		return
	}

	// Authenticate before saying anything about whether the resource exists:
	// otherwise the endpoint is a free directory of a pipeline's resource
	// names to anyone who can reach it.
	if !h.authorized(name, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	enqueued, err := h.checkNow(r.Context(), name)
	if err != nil {
		slog.Warn("webhook.check_failed", "resource", name, "error", err)
		http.Error(w, "check failed", http.StatusInternalServerError)

		return
	}

	printf("webhook: %s checked, enqueued %v\n", name, enqueued)

	// The response body deliberately echoes nothing back: the request path is
	// attacker-controlled, and the sender already knows what it asked for.
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok\n")
}

// authorized compares the presented token against the one the named
// resource's environment variable holds, in constant time.
func (h *webhookHandler) authorized(name string, r *http.Request) bool {
	envVar, ok := h.tokens[name]
	if !ok {
		return false
	}

	expected := os.Getenv(envVar)
	if expected == "" {
		// A resource whose token variable is unset accepts nothing. Treating
		// an empty expectation as "no auth required" would turn a
		// misconfigured deployment into an open trigger endpoint.
		slog.Warn("webhook.token_unset", "resource", name, "env", envVar)

		return false
	}

	presented := r.URL.Query().Get("token")
	if presented == "" {
		presented = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	}

	return subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) == 1
}

// checkNow checks one resource immediately and enqueues whatever it affects,
// reusing exactly the poll path so a webhook and a poll cannot disagree about
// what a version change means.
func (h *webhookHandler) checkNow(ctx context.Context, name string) ([]string, error) {
	obs, hasVersion, err := checkResource(ctx, h.cfg, h.st, name)
	if err != nil {
		return nil, err
	}

	if !hasVersion {
		return nil, nil
	}

	// A webhook says "something happened", so the version is treated as dirty
	// even when it matches what was last recorded — the sender knows something
	// the check output may not show yet.
	obs.dirty = true

	enqueued, err := enqueueAffected(ctx, h.cfg, h.st, map[string]observedResource{name: obs})
	if err != nil {
		return nil, err
	}

	err = h.st.RecordCheckedVersion(ctx, name, obs.latest)
	if err != nil {
		return enqueued, fmt.Errorf("record version for %q: %w", name, err)
	}

	return enqueued, nil
}

// WebhookHandler serves POST .../check/<resource> for the resources this
// pipeline has given a webhook_token_env:, or nil when it has given none.
//
// A handler rather than a server: `steps watch --listen` (before the daemons merged) used to open a
// second port of its own beside the UI, so a pipeline that wanted both had
// two addresses, two flavors of HTTP surface and two things to expose. There
// is one listener now — the web UI mounts this under the pipeline it belongs
// to — and nil is how a caller learns there is nothing to mount, which is
// different from mounting an endpoint that refuses everything.
func WebhookHandler(cfg *config.Config, st *store.Store) http.Handler {
	tokens := cfg.WebhookResources()
	if len(tokens) == 0 {
		return nil
	}

	return &webhookHandler{cfg: cfg, st: st, tokens: tokens}
}
