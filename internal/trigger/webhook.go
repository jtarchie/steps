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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/store"
)

// webhookShutdownGrace bounds how long a shutdown waits for in-flight webhook
// requests before giving up on them.
const webhookShutdownGrace = 5 * time.Second

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

	name := strings.TrimPrefix(r.URL.Path, "/check/")
	if name == "" || strings.Contains(name, "/") {
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

// serveWebhooks runs the webhook listener until ctx is done.
//
// It returns an error only for a listener that could not start or died
// unexpectedly: a webhook endpoint that silently is not listening is worse
// than one that refuses to start, because the pipeline looks configured and
// reacts to nothing.
func serveWebhooks(ctx context.Context, cfg *config.Config, st *store.Store, addr string) error {
	tokens := cfg.WebhookResources()
	if len(tokens) == 0 {
		return errors.New("--listen was given but no resource sets webhook_token_env:; nothing could ever be triggered")
	}

	mux := http.NewServeMux()
	mux.Handle("/check/", &webhookHandler{cfg: cfg, st: st, tokens: tokens})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
		// A webhook body is tiny and a sender that stalls mid-request must not
		// hold a connection open indefinitely.
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhookShutdownGrace)
		defer cancel()

		_ = server.Shutdown(shutdownCtx)
	}()

	printf("webhook: listening on %s for %d resource(s)\n", addr, len(tokens))
	slog.Info("webhook.listening", "addr", addr, "resources", len(tokens))

	err := server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("webhook listener: %w", err)
	}

	return nil
}
