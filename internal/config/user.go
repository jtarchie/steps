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

// validateUserValues rejects a user: value starting with '-', which no user
// name or uid does, so it can only be a mistake — refused at load rather than
// when the container is created.
func (c *Config) validateUserValues() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		return checkUserValue(context, settings.User)
	})
}

// checkUserValue rejects a user: value beginning with '-'.
func checkUserValue(context, user string) error {
	if strings.HasPrefix(user, "-") {
		return fmt.Errorf("%s: user %q must not start with '-' (no user name or uid does)", context, user)
	}

	return nil
}
