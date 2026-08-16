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
	err := c.visitContainerSettings(func(context string, settings containerSettings) error {
		return checkEnvNames(context, settings.Env)
	})
	if err != nil {
		return err
	}

	// Resource.Env is not a containerSettings field — a resource instance
	// doesn't run in a container, only its type's check/in/out do — so it
	// gets its own small walk rather than joining visitContainerSettings.
	for i := range c.Resources {
		res := c.Resources[i]

		err := checkEnvNames(fmt.Sprintf("resource %q", res.Name), res.Env)
		if err != nil {
			return err
		}

		err = c.validateResourceEnvBackend(res)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateResourceEnvBackend rejects env: on a resource whose type is
// mcp-backed. An mcp-backed type authenticates via its mcp_servers: entry
// and never calls env() at all (see resource.CheckVersions/RunIn/RunOut,
// whose BackendMCP arm doesn't thread extraEnv through), so env: there is
// not a smaller privilege grant — it is silently inert. Better to say so at
// load time than let a copied reply-as-support-bot-style pattern (env: +
// source.token_env) pass validation, run green, and never actually widen
// anything.
func (c *Config) validateResourceEnvBackend(res Resource) error {
	if len(res.Env) == 0 {
		return nil
	}

	resourceType, err := c.FindResourceType(res.Type)
	if err != nil {
		return nil //nolint:nilerr // unresolvable resource type is caught elsewhere at run time
	}

	if resourceType.Config.Backend() == BackendMCP {
		return fmt.Errorf(
			"resource %q: env: has no effect — resource type %q is mcp-backed, which authenticates via its mcp_servers: entry and never reads env: at all; drop env: here",
			res.Name, res.Type)
	}

	return nil
}

// checkEnvNames applies checkEnvName across one env: list, shared by
// visitContainerSettings' callback and the Resource.Env walk above so the
// two sites can't drift on how a bad entry is reported.
func checkEnvNames(context string, names []string) error {
	for _, name := range names {
		err := checkEnvName(context, name)
		if err != nil {
			return err
		}
	}

	return nil
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
