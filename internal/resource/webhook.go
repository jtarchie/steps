package resource

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/exprlang"
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

	if !slices.Contains(webhook.ProviderNames(), source.Provider) {
		return nil, fmt.Errorf("resource %q: unknown webhook provider %q (known: %s)",
			res.Name, source.Provider, strings.Join(webhook.ProviderNames(), ", "))
	}

	receiver := &webhook.Receiver{Provider: source.Provider, SecretEnv: source.SecretEnv, MaxBody: source.MaxBody}
	compiler := webhookCompiler{resource: res.Name}

	receiver.Filter = compiler.filter(source.Filter)
	receiver.ID = compiler.text("id", source.ID)
	receiver.Scheme = compiler.scheme(source.Signature)

	for name, src := range source.Version {
		if receiver.Version == nil {
			receiver.Version = map[string]func(webhook.Input) (string, error){}
		}

		receiver.Version[name] = compiler.text("version."+name, src)
	}

	return receiver, errors.Join(compiler.errs...)
}

// webhookCompiler collects every expression error in one resource, so one set names them all.
type webhookCompiler struct {
	resource string
	errs     []error
}

func (c *webhookCompiler) compile(field, src string, wantBool bool) func(webhook.Input) (any, error) {
	run, err := exprlang.Webhook(src, wantBool)
	if err != nil {
		c.errs = append(c.errs, fmt.Errorf("resource %q: source.%s: %w", c.resource, field, err))

		return nil
	}

	return func(in webhook.Input) (any, error) {
		return run(exprlang.WebhookEnv(in))
	}
}

func (c *webhookCompiler) filter(src string) func(webhook.Input) (bool, error) {
	if src == "" {
		return nil
	}

	run := c.compile("filter", src, true)

	return func(in webhook.Input) (bool, error) {
		result, err := run(in)
		keep, _ := result.(bool)

		return keep, err
	}
}

func (c *webhookCompiler) text(field, src string) func(webhook.Input) (string, error) {
	if src == "" {
		return nil
	}

	run := c.compile(field, src, false)

	return func(in webhook.Input) (string, error) {
		result, err := run(in)
		if err != nil {
			return "", err
		}

		return webhookString(result)
	}
}

func (c *webhookCompiler) scheme(signature *config.WebhookSignature) *webhook.Scheme {
	if signature == nil {
		return nil
	}

	return &webhook.Scheme{
		Header:    signature.Header,
		Prefix:    signature.Prefix,
		Encoding:  signature.Encoding,
		Algorithm: signature.Algorithm,
		Signed:    c.text("signature.signed", signature.Signed),
		Timestamp: c.text("signature.timestamp", signature.Timestamp),
	}
}

// webhookString is a version field as a string. A JSON number decodes to float64, and an id like 3370172613 must not come back as 3.370172613e+09.
func webhookString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(v), nil
	case bool:
		return strconv.FormatBool(v), nil
	default:
		return "", fmt.Errorf("evaluated to %T, want a string", value)
	}
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
