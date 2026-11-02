package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/secret"
	"github.com/jtarchie/steps/internal/shell"
)

// shortSecret is the length below which masking a value starts catching ordinary text.
const shortSecret = 8

type secretsKey struct{}

type runSecretsKey struct{}

// WithSecrets hands the daemon's sources to every run made under the context.
func WithSecrets(ctx context.Context, resolver *secret.Resolver) context.Context {
	return context.WithValue(ctx, secretsKey{}, resolver)
}

// CheckSecretSources refuses a reference to a source this daemon does not configure; it never runs one, since any local process can reach set's loopback endpoint and must not be able to trigger every prompt on the operator's screen.
func CheckSecretSources(ctx context.Context, cfg *config.Config) error {
	resolver, _ := ctx.Value(secretsKey{}).(*secret.Resolver)

	for _, ref := range cfg.SecretRefs() {
		if resolver == nil || !resolver.Has(ref.Source) {
			return fmt.Errorf("%s: no secret source %q is configured on this daemon (steps web --config, secret: section)", ref, ref.Source)
		}
	}

	return nil
}

// withRunSecrets starts the run's memo, so every step of the run shares one lookup per reference.
func withRunSecrets(ctx context.Context) context.Context {
	resolver, _ := ctx.Value(secretsKey{}).(*secret.Resolver)
	if resolver == nil {
		return ctx
	}

	ctx = redact.WithSecrets(ctx, &redact.Secrets{})

	return context.WithValue(ctx, runSecretsKey{}, resolver.ForRun())
}

// withStepSecrets resolves a step's secrets: map onto the context its commands run under; lazily, so a branch that never runs fetches nothing.
func withStepSecrets(ctx context.Context, secrets map[string]string) (context.Context, error) {
	if len(secrets) == 0 {
		return ctx, nil
	}

	run, _ := ctx.Value(runSecretsKey{}).(*secret.Run)
	if run == nil {
		return ctx, errors.New("secrets: this run has no secret source; only steps web resolves secrets")
	}

	meta := shell.MetadataFrom(ctx)
	env := make(map[string]string, len(secrets))

	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		ref, err := config.ParseSecretRef(secrets[name])
		if err != nil {
			return ctx, fmt.Errorf("secrets.%s: %w", name, err)
		}

		value, err := run.Lookup(ctx, secret.Ref{Pipeline: meta.PipelineName, Job: meta.JobName, Source: ref.Source, Name: ref.Name})
		if err != nil {
			return ctx, fmt.Errorf("secrets.%s: %w", name, err)
		}

		// A short value still masks, and masks unrelated output too; said once so a mangled log has an explanation.
		if len(value) < shortSecret {
			slog.WarnContext(ctx, "secret.short", "secret", ref.String(), "bytes", len(value), "note", "masking it may also mask unrelated output")
		}

		// Registered before the step runs, so its first byte of output is already scrubbed.
		redact.SecretsFrom(ctx).Add(value)

		env[name] = value
	}

	return shell.WithSecretEnv(ctx, env), nil
}
