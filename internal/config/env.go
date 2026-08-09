package config

// The env: field wherever it appears: which host environment variables a
// pipeline-defined command is allowed to see.

import (
	"fmt"
	"strings"
)

// validateEnvRules groups the env:-related load-time checks, so
// config.validate's own branch count doesn't grow with every env: rule added
// — mirroring validateImageRules.
func (c *Config) validateEnvRules() error {
	err := c.validateEnvValues()
	if err != nil {
		return err
	}

	// Non-nil, not non-empty: an explicit `env: []` is a real declaration
	// ("nothing beyond the baseline"), so writing one on a get/put step is
	// still the mistake this rejects.
	return c.rejectOnGetAndPut("env", func(s *Step) bool { return s.Env != nil })
}

// validateEnvValues rejects an env: entry that carries a value rather than
// naming one.
//
// env: is a list of variable NAMES, resolved from the operator's own
// environment when the command runs — the same name-not-value shape
// api_key_env: and webhook_token_env: use, and for the same reason: a
// resource's, task's, and agent's fields are hashed into the merkle content
// map, so a literal secret written here would be persisted to state.db in
// cleartext. Accepting "KEY=value" silently would put the value in exactly
// the place the whole convention exists to keep it out of.
func (c *Config) validateEnvValues() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		for _, name := range settings.Env {
			err := checkEnvName(context, name)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// checkEnvName rejects an env: entry that isn't a bare variable name.
func checkEnvName(context, name string) error {
	if name == "" {
		return fmt.Errorf("%s: env contains an empty variable name", context)
	}

	if strings.Contains(name, "=") {
		return fmt.Errorf("%s: env entry %q must be a variable NAME, not a KEY=VALUE pair — the value is read from the environment at run time (a literal would be hashed into state.db in cleartext)", context, name)
	}

	return nil
}
