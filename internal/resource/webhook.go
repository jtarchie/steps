package resource

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/webhook"
)

// errWebhookIn is a get of a webhook resource that reached RunIn: its files are the recorded delivery, which only the caller holding the store can write.
var errWebhookIn = errors.New("a webhook resource is fetched from its recorded delivery, not by running in")

// Receiver compiles one webhook resource into what internal/webhook runs. Every door a pipeline comes in by calls it, so a mistake is refused where the pipeline is set rather than at the first delivery.
func Receiver(res config.Resource) (*webhook.Receiver, error) {
	source, err := res.WebhookSource()
	if err != nil {
		return nil, err //nolint:wrapcheck // WebhookSource names the resource
	}

	if _, known := webhook.Providers[source.Provider]; !known {
		return nil, fmt.Errorf("resource %q: unknown webhook provider %q (known: %s)",
			res.Name, source.Provider, strings.Join(slices.Sorted(maps.Keys(webhook.Providers)), ", "))
	}

	return &webhook.Receiver{Provider: source.Provider, SecretEnv: source.SecretEnv, MaxBody: source.MaxBody}, nil
}

// CompileWebhooks is Receiver over every webhook resource, for a caller that wants only the refusal.
func CompileWebhooks(cfg *config.Config) error {
	for _, res := range cfg.Resources {
		if res.Type != config.WebhookType {
			continue
		}

		_, err := Receiver(res)
		if err != nil {
			return err
		}
	}

	return nil
}
