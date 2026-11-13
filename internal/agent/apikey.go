package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/steps/internal/agent/llmclient"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// KeyLookup resolves a secret-backed model key: name is the variable api_key_env names, ref its ((source:name)) reference. A hook, because depguard keeps internal/secret out of this package.
type KeyLookup func(ctx context.Context, name, ref string) (string, error)

type keyLookupKey struct{}

type keySource struct {
	pipeline string
	lookup   KeyLookup
}

// WithKeyLookup installs how this context resolves a secret-backed model key; pipeline is for a context with no build metadata to name it (a poll), as in resource.WithSecretLookup's installer.
func WithKeyLookup(ctx context.Context, pipeline string, lookup KeyLookup) context.Context {
	return context.WithValue(ctx, keyLookupKey{}, keySource{pipeline: pipeline, lookup: lookup})
}

// errKeyLookup marks a failure to FETCH a key, so the probe cache does not remember it: a declined prompt or a lapsed login is retried by the next run, as every other secret lookup is.
var errKeyLookup = errors.New("model key")

// apiKeyFor is the one place a model's key is read: through the context's lookup when the source names a secret, and from the environment exactly as before otherwise.
func apiKeyFor(ctx context.Context, ri config.ResolvedInvocation) (string, error) {
	if ri.APIKeySecret == "" {
		key, err := llmclient.LookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
		if err != nil {
			return "", fmt.Errorf("%w", err)
		}

		return key, nil
	}

	source, _ := ctx.Value(keyLookupKey{}).(keySource)
	if source.lookup == nil {
		return "", fmt.Errorf("%w: source.secrets.%s: no secret source here; steps web, or steps run --config, resolves secrets", errKeyLookup, ri.APIKeyEnv)
	}

	key, err := source.lookup(ctx, ri.APIKeyEnv, ri.APIKeySecret)
	if err != nil {
		return "", fmt.Errorf("%w: source.%w", errKeyLookup, err)
	}

	return key, nil
}

// keyProbeScope is what a secret-backed source adds to its probe cache key: the same variable name resolves to a different credential per pipeline, so one pipeline's answer must not vouch for another's.
func keyProbeScope(ctx context.Context, ri config.ResolvedInvocation) string {
	if ri.APIKeySecret == "" {
		return ""
	}

	source, _ := ctx.Value(keyLookupKey{}).(keySource)

	return "|" + cmp.Or(shell.MetadataFrom(ctx).PipelineName, source.pipeline) + "|" + ri.APIKeySecret
}
