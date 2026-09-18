package resource

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestAnUnknownProviderIsRefusedAtLoad: the provider is declared, never detected, so a name the table does not hold is a mistake to refuse where the pipeline is set, not a scheme to guess at the first delivery.
func TestAnUnknownProviderIsRefusedAtLoad(t *testing.T) {
	cfg := &config.Config{Resources: []config.Resource{{
		Name: "push", Type: config.WebhookType,
		Source: map[string]any{"provider": "gihtub", "secret_env": "SECRET"},
	}}}

	err := CompileWebhooks(cfg)
	if err == nil || !strings.Contains(err.Error(), `unknown webhook provider "gihtub"`) {
		t.Errorf("err = %v, want the unknown provider named", err)
	}
}
