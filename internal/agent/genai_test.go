package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestEveryCLISourceNamesItsProvider: a CLI added to config without an entry here would export its invoke_agent with no gen_ai.provider.name.
func TestEveryCLISourceNamesItsProvider(t *testing.T) {
	t.Parallel()

	for _, name := range config.CLIProviderNames() {
		if cliProviders[name] == "" {
			t.Errorf("cli %q has no gen_ai.provider.name in cliProviders", name)
		}
	}
}
