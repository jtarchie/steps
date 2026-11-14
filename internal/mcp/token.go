package mcp

import (
	"cmp"
	"context"
	"fmt"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// TokenLookup resolves a secret-backed bearer token: name is the variable api_key_env names, ref its ((source:name)) reference. A hook, because depguard keeps internal/secret out of this package.
type TokenLookup func(ctx context.Context, name, ref string) (string, error)

type tokenLookupKey struct{}

type tokenSource struct {
	pipeline string
	lookup   TokenLookup
}

// WithTokenLookup installs how this context resolves a secret-backed bearer token; pipeline is for a context with no build metadata to name it (a poll), as in agent.WithKeyLookup.
func WithTokenLookup(ctx context.Context, pipeline string, lookup TokenLookup) context.Context {
	return context.WithValue(ctx, tokenLookupKey{}, tokenSource{pipeline: pipeline, lookup: lookup})
}

// bearerTokenFor is the one place a bearer token is read: through the context's lookup when the server names a secret, and from the environment exactly as before otherwise.
func bearerTokenFor(ctx context.Context, srv config.MCPServer) (string, error) {
	ref := srv.Auth.TokenSecret()
	if ref == "" {
		return lookupBearerToken(srv.Auth.APIKeyEnv)
	}

	source, _ := ctx.Value(tokenLookupKey{}).(tokenSource)
	if source.lookup == nil {
		return "", fmt.Errorf("$%s is a secret, and there is no secret source here: pass --config", srv.Auth.APIKeyEnv)
	}

	token, err := source.lookup(ctx, srv.Auth.APIKeyEnv, ref)
	if err != nil {
		return "", fmt.Errorf("auth.%w", err)
	}

	return token, nil
}

// TokenScope is what a secret-backed server adds to a probe cache key: the same definition resolves to a different credential per pipeline, so one pipeline's verdict must not vouch for another's.
func TokenScope(ctx context.Context, srv config.MCPServer) string {
	ref := srv.Auth.TokenSecret()
	if ref == "" {
		return ""
	}

	source, _ := ctx.Value(tokenLookupKey{}).(tokenSource)

	return cmp.Or(shell.MetadataFrom(ctx).PipelineName, source.pipeline) + "|" + ref
}
