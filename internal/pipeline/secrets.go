package pipeline

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/redact"
	"github.com/jtarchie/steps/internal/resource"
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

	return withSecretMemo(ctx, resolver.ForRun(), "")
}

// WithPollSecrets is the poller's resolution for one pipeline: no run, so answers are cached for ttl rather than per run.
func WithPollSecrets(ctx context.Context, pipelineName string, ttl time.Duration) context.Context {
	resolver, _ := ctx.Value(secretsKey{}).(*secret.Resolver)
	if resolver == nil {
		return ctx
	}

	return withSecretMemo(ctx, resolver.ForChecks(ttl), pipelineName)
}

// withSecretMemo installs memo for steps and resources alike; pipelineName is for a context with no build metadata to read it from (a poll).
func withSecretMemo(ctx context.Context, memo *secret.Run, pipelineName string) context.Context {
	secrets := &redact.Secrets{}
	ctx = redact.WithSecrets(ctx, secrets)
	ctx = events.WithScrub(ctx, secrets.String)
	ctx = context.WithValue(ctx, runSecretsKey{}, memo)

	return resource.WithSecretLookup(ctx, func(ctx context.Context, name string, secrets map[string]string) (map[string]string, error) {
		return lookupSecrets(ctx, secret.Ref{Pipeline: cmp.Or(shell.MetadataFrom(ctx).PipelineName, pipelineName), Job: shell.MetadataFrom(ctx).JobName, Resource: name}, secrets)
	})
}

// withStepSecrets resolves a step's secrets: map onto the context its commands run under; lazily, so a branch that never runs fetches nothing.
func withStepSecrets(ctx context.Context, secrets map[string]string) (context.Context, error) {
	if len(secrets) == 0 {
		return ctx, nil
	}

	meta := shell.MetadataFrom(ctx)

	env, err := lookupSecrets(ctx, secret.Ref{Pipeline: meta.PipelineName, Job: meta.JobName}, secrets)
	if err != nil {
		return ctx, err
	}

	return shell.WithSecretEnv(ctx, env), nil
}

// lookupSecrets resolves each name of a secrets: map through the context's memo, scoped by where (pipeline, job, resource), and registers each value for scrubbing before anything runs.
func lookupSecrets(ctx context.Context, where secret.Ref, secrets map[string]string) (map[string]string, error) {
	run, _ := ctx.Value(runSecretsKey{}).(*secret.Run)
	if run == nil {
		return nil, errors.New("secrets: this run has no secret source; only steps web resolves secrets")
	}

	env := make(map[string]string, len(secrets))

	for _, name := range slices.Sorted(maps.Keys(secrets)) {
		ref, err := config.ParseSecretRef(secrets[name])
		if err != nil {
			return nil, fmt.Errorf("secrets.%s: %w", name, err)
		}

		where.Source, where.Name = ref.Source, ref.Name

		value, err := run.Lookup(ctx, where)
		if err != nil {
			return nil, fmt.Errorf("secrets.%s: %w", name, err)
		}

		// A short value still masks, and masks unrelated output too; said once so a mangled log has an explanation.
		if len(value) < shortSecret {
			slog.WarnContext(ctx, "secret.short", "secret", ref.String(), "bytes", len(value), "note", "masking it may also mask unrelated output")
		}

		redact.SecretsFrom(ctx).Add(value)

		env[name] = value
	}

	return env, nil
}
