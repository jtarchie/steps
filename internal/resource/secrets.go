package resource

import (
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// SecretLookup resolves one resource's secrets: map to values. Whoever owns the lookup's lifetime installs it: a run memoizes per run, the poller caches for an interval.
type SecretLookup func(ctx context.Context, resource string, secrets map[string]string) (map[string]string, error)

type secretLookupKey struct{}

// WithSecretLookup installs how this context resolves a resource's secrets.
func WithSecretLookup(ctx context.Context, lookup SecretLookup) context.Context {
	return context.WithValue(ctx, secretLookupKey{}, lookup)
}

// LookupSecrets resolves a secrets: map for the named resource through the context's lookup, for a reader with no check, in or out to run it under: a webhook's delivery.
func LookupSecrets(ctx context.Context, resource string, secrets map[string]string) (map[string]string, error) {
	lookup, _ := ctx.Value(secretLookupKey{}).(SecretLookup)
	if lookup == nil {
		return nil, errors.New("secrets: no secret source here; only steps web resolves secrets")
	}

	env, err := lookup(ctx, resource, secrets)
	if err != nil {
		return nil, fmt.Errorf("resource %q: %w", resource, err)
	}

	return env, nil
}

// withResourceSecrets puts a resource's resolved secrets on the context its check, in or out runs under; every backend reads them from there.
func withResourceSecrets(ctx context.Context, r config.Resource) (context.Context, error) {
	// A webhook's one secret verifies its deliveries, which a get of one is long past: fetching it for a run would prompt for a value nothing reads.
	if len(r.Secrets) == 0 || r.Type == config.WebhookType {
		return ctx, nil
	}

	env, err := LookupSecrets(ctx, r.Name, r.Secrets)
	if err != nil {
		return ctx, err
	}

	return shell.WithSecretEnv(ctx, env), nil
}
