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
	// current is read per request, not captured, for the reason Poll takes a
	// source: the daemon swaps its configuration while this handler is
	// mounted. A captured one kept authenticating against a token env var the
	// operator had deleted and checking a resource definition the file no
	// longer held.
	current ConfigSource
	st      *store.Store
	// base is the daemon's context: the --worker map and artifact store a
	// placed check resolves through. A request's own context carries none
	// of it — the server minted it — so a tagged resource's webhook checked
	// from nowhere and failed.
	base context.Context //nolint:containedctx // the values, not the lifetime; see requestContext
}

// requestContext is a request's context for cancellation and the daemon's for
// values: what the invocation knows, ended when the sender goes away.
type requestContext struct {
	context.Context //nolint:containedctx // it IS a context: the request's, with a second source of values

	base context.Context //nolint:containedctx // the fallback for values only
}

func (c requestContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil || c.base == nil {
		return value
	}

	return c.base.Value(key)
}

func (h *webhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// A GET would be triggerable from a browser preview or a link
		// scanner, which is not a thing a pipeline should be startable by.
		http.Error(w, "use POST", http.StatusMethodNotAllowed)

		return
	}

	cfg := h.current()

	tokens := cfg.WebhookResources()
	if len(tokens) == 0 {
		// The pipeline as it stands names no webhook_token_env: resource, so
		// there is nothing here to trigger — the same answer the server gave
		// when this was decided once, at mount time, and now an answer that
		// follows a reload in both directions.
		http.Error(w, "no webhook resources in this pipeline", http.StatusNotFound)

		return
	}

	// The last segment of .../check/<resource>, found from the END rather
	// than by trimming a fixed prefix: this handler is mounted BY its server,
	// and the web UI mounts it under the pipeline it belongs to
	// (/p/<slug>/check/<resource>) — a pipeline-blind route would be
	// ambiguous the moment one process serves two pipelines.
	//
	// LastIndex, not Cut: Cut splits on the FIRST match, so a pipeline whose
	// slug is literally `check` gave /p/check/check/<resource> a name of
	// "check/<resource>", which the slash guard below then 404s — every
	// correctly-signed delivery, silently, forever.
	name := ""
	found := false

	if at := strings.LastIndex(r.URL.Path, "/check/"); at >= 0 {
		name, found = r.URL.Path[at+len("/check/"):], true
	}

	if !found || name == "" || strings.Contains(name, "/") {
		http.Error(w, "not found", http.StatusNotFound)

		return
	}

	// Authenticate before saying anything about whether the resource exists:
	// otherwise the endpoint is a free directory of a pipeline's resource
	// names to anyone who can reach it.
	if !authorized(tokens, name, r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return
	}

	//nolint:contextcheck // the request's own context, with the daemon's values behind it — see requestContext
	enqueued, err := h.checkNow(requestContext{Context: r.Context(), base: h.base}, cfg, name)
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
//
// tokens is passed rather than held, so the answer comes from the
// configuration being served at this instant and not the one this handler was
// mounted with.
func authorized(tokens map[string]string, name string, r *http.Request) bool {
	envVar, ok := tokens[name]
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
func (h *webhookHandler) checkNow(ctx context.Context, cfg *config.Config, name string) ([]string, error) {
	ctx, release := leasedChecks(ctx)
	defer release()

	obs, hasVersion, err := checkResource(ctx, cfg, h.st, name)
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

	enqueued, err := enqueueAffected(ctx, cfg, h.st, map[string]observedResource{name: obs})
	if err != nil {
		return nil, err
	}

	err = h.st.RecordCheckedVersion(ctx, name, obs.latest)
	if err != nil {
		return enqueued, fmt.Errorf("record version for %q: %w", name, err)
	}

	return enqueued, nil
}

// WebhookHandler serves POST .../check/<resource> for the resources a
// pipeline has given a webhook_token_env:.
//
// A handler rather than a server: the poll loop used to open a second port of its
// own beside the UI, so a pipeline that wanted both had two addresses, two
// flavors of HTTP surface and two things to expose. There
// is one listener now — the web UI mounts this under the pipeline it belongs
// to.
//
// It takes a SOURCE, and "is there anything here to trigger?" is answered per
// request rather than once, at mount. Deciding it once was correct only while
// a daemon's configuration could not change: a pipeline that GAINED its first
// webhook resource by edit was mounted as nil and 404'd forever, and one that
// LOST a resource kept an endpoint live that authenticated against a token
// env var the operator believed they had deleted.
func WebhookHandler(base context.Context, current ConfigSource, st *store.Store) http.Handler {
	return &webhookHandler{current: current, st: st, base: base}
}
