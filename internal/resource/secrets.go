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

// withResourceSecrets puts a resource's resolved secrets on the context its check, in or out runs under; every backend reads them from there.
func withResourceSecrets(ctx context.Context, r config.Resource) (context.Context, error) {
	if len(r.Secrets) == 0 {
		return ctx, nil
	}

	lookup, _ := ctx.Value(secretLookupKey{}).(SecretLookup)
	if lookup == nil {
		return ctx, errors.New("secrets: no secret source here; only steps web resolves secrets")
	}

	env, err := lookup(ctx, r.Name, r.Secrets)
	if err != nil {
		return ctx, fmt.Errorf("resource %q: %w", r.Name, err)
	}

	return shell.WithSecretEnv(ctx, env), nil
}
