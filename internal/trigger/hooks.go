package trigger

// POST /p/<name>/hooks/<resource>: a webhook delivery, recorded as a version of the resource it was sent to.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jtarchie/steps/internal/config"
	rsrc "github.com/jtarchie/steps/internal/resource"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/webhook"
)

// HookStore is what a delivery touches: the jobs it may queue, whether the pipeline is paused, and the delivery itself.
type HookStore interface {
	PollStore
	store.Deliveries
}

// HookHandler answers deliveries to one pipeline's webhook resources. The configuration is read per request, as Poll reads it per cycle, because a set swaps it while this stays mounted.
func HookHandler(current ConfigSource, st HookStore) func(w http.ResponseWriter, r *http.Request, resource string) {
	return func(w http.ResponseWriter, r *http.Request, resource string) {
		receive(w, r, current(), st, resource)
	}
}

// receive runs a delivery in the order that keeps an unverified one from reaching anything the pipeline wrote: verify, then the pipeline's expressions, then one transaction.
func receive(w http.ResponseWriter, r *http.Request, cfg *config.Config, st HookStore, name string) {
	receiver, found := receiverFor(w, cfg, name)
	if !found {
		return
	}

	body, valid := verified(w, r, receiver, name)
	if !valid {
		return
	}

	result, err := receiver.Accept(r.Method, r.Header, r.URL.Query(), body)
	if err != nil {
		// A 500 the sender logs as failed, so it can be redelivered once the pipeline is fixed; nothing is recorded meanwhile.
		slog.Error("webhook.expr_error", "resource", name, "error", err)
		http.Error(w, "the pipeline could not evaluate this delivery", http.StatusInternalServerError)

		return
	}

	switch {
	case result.Handshake != nil:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.Handshake)
	case result.Filtered:
		printf("webhook: %s filtered out a delivery\n", name)
		ok(w)
	default:
		record(w, r, cfg, st, name, result.Delivery)
	}
}

func receiverFor(w http.ResponseWriter, cfg *config.Config, name string) (*webhook.Receiver, bool) {
	if !hasWebhookResources(cfg) {
		http.Error(w, "no webhook resources in this pipeline", http.StatusNotFound)

		return nil, false
	}

	// Unknown and not-a-webhook answer exactly as a bad signature does, so the route does not enumerate a pipeline's resources for whoever asks.
	res, err := cfg.FindResource(name)
	if err != nil || res.Type != config.WebhookType {
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return nil, false
	}

	receiver, err := rsrc.Receiver(*res)
	if err != nil {
		slog.Error("webhook.config", "resource", name, "error", err)
		http.Error(w, "webhook misconfigured", http.StatusInternalServerError)

		return nil, false
	}

	return receiver, true
}

func verified(w http.ResponseWriter, r *http.Request, receiver *webhook.Receiver, name string) ([]byte, bool) {
	body, err := receiver.ReadBody(w, r)
	if errors.Is(err, webhook.ErrTooLarge) {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)

		return nil, false
	}

	if err != nil {
		http.Error(w, "could not read the body", http.StatusBadRequest)

		return nil, false
	}

	secret := os.Getenv(receiver.SecretEnv)
	if secret == "" {
		slog.Warn("webhook.secret_unset", "resource", name, "env", receiver.SecretEnv)
	}

	err = receiver.Verify(r.Header, body, secret, time.Now())
	if err != nil {
		slog.Info("webhook.unauthorized", "resource", name)
		http.Error(w, "unauthorized", http.StatusUnauthorized)

		return nil, false
	}

	return body, true
}

func record(w http.ResponseWriter, r *http.Request, cfg *config.Config, st HookStore, name string, accepted webhook.Delivery) {
	dispatch, err := deliveryDispatch(r, cfg, st, name)
	if err != nil {
		slog.Error("webhook.enqueue", "resource", name, "error", err)
		http.Error(w, "could not record the delivery", http.StatusInternalServerError)

		return
	}

	delivery := store.Delivery{Version: accepted.Version, Body: accepted.Body, Headers: accepted.Headers}

	recorded, err := st.RecordDelivery(r.Context(), name, delivery, dispatch, cfg.VersionHistoryLimit())
	if err != nil {
		// Not a 2xx: the sender retries, rather than an event being lost to a write that failed.
		slog.Error("webhook.record", "resource", name, "error", err)
		http.Error(w, "could not record the delivery", http.StatusInternalServerError)

		return
	}

	switch {
	case !recorded:
		printf("webhook: %s received a delivery it already has (%v); nothing enqueued\n", name, delivery.Version["id"])
	case dispatch.Held:
		printf("webhook: %s recorded %v while the pipeline is paused; it builds on unpause\n", name, delivery.Version["id"])
	default:
		printf("webhook: %s recorded %v, enqueued %v\n", name, delivery.Version["id"], dispatch.Jobs)
	}

	ok(w)
}

// deliveryDispatch is what a delivery queues: every trigger: true job whose passed: constraints hold — or, while the pipeline is paused, nothing yet.
func deliveryDispatch(r *http.Request, cfg *config.Config, st HookStore, name string) (store.Dispatch, error) {
	paused, err := st.Paused(r.Context())
	if err != nil || paused {
		return store.Dispatch{Held: paused}, err //nolint:wrapcheck // logged with the resource by the caller
	}

	var dispatch store.Dispatch

	for _, job := range AffectedJobs(cfg, name) {
		ready, err := jobReadyFor(r.Context(), st, job)
		if err != nil {
			return dispatch, err
		}

		if ready {
			dispatch.Jobs = append(dispatch.Jobs, job.Name)
		}
	}

	return dispatch, nil
}

// observeDeliveries is a webhook resource's turn in a poll: no check, only whether the newest delivery has been dispatched. A delivery normally dispatches itself as it is recorded, so this finds work only after a pause held one back. It is at-least-once rather than exact: a delivery landing between this read and the poll's write of the current version is dispatched again by the next poll, which the queue's one-pending-row-per-job absorbs.
func observeDeliveries(ctx context.Context, st PollStore, name string) (observedResource, bool, error) {
	versions, err := st.ResourceVersionsJSON(ctx, name)
	if err != nil || len(versions) == 0 {
		return observedResource{}, false, err //nolint:wrapcheck // the store names the resource
	}

	latest := versions[len(versions)-1]

	version, err := store.DecodeVersion(latest)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("webhook resource %q: %w", name, err)
	}

	previous, found, err := st.LastChecked(ctx, name)
	if err != nil {
		return observedResource{}, false, fmt.Errorf("webhook resource %q: %w", name, err)
	}

	return observedResource{version: version, latest: latest, dirty: !found || previous.Version != latest}, true, nil
}

func isWebhook(cfg *config.Config, name string) bool {
	for _, res := range cfg.Resources {
		if res.Name == name {
			return res.Type == config.WebhookType
		}
	}

	return false
}

func hasWebhookResources(cfg *config.Config) bool {
	for _, res := range cfg.Resources {
		if res.Type == config.WebhookType {
			return true
		}
	}

	return false
}
