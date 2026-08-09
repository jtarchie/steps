package config

// The user: field wherever it appears: which container user a command runs as.

import (
	"fmt"
	"strings"
)

// validateUserRules groups the user:-related load-time checks, mirroring
// validateImageRules so config.validate's branch count doesn't grow per rule.
func (c *Config) validateUserRules() error {
	err := c.validateUserValues()
	if err != nil {
		return err
	}

	return c.rejectOnGetAndPut("user", func(s *Step) bool { return s.User != "" })
}

// validateUserValues rejects a user: value docker's parser would read as a
// flag rather than a user spec. Exactly the reasoning behind
// validateImageValues, but load-bearing in a way that one is not: user: is
// passed as `--user <value>` BEFORE the "--" that makes the rest of the argv
// positional, so unlike image: there is no separator behind it. This check is
// the only thing standing between a mistyped or tainted user: and docker
// granting whatever that flag means.
func (c *Config) validateUserValues() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		return checkUserValue(context, settings.User)
	})
}

// checkUserValue rejects a user: value beginning with '-'.
func checkUserValue(context, user string) error {
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("%s: user %q must not start with '-' (docker would parse it as a flag, not a user)", context, user)
	}

	return nil
}
