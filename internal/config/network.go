package config

// The network: field wherever it appears: what a containerized command can
// reach.

import (
	"fmt"
	"strings"
)

// noNetwork is docker's "no egress at all" network. Named because two rules
// turn on it specifically (see validateNetworkNeedsImage and
// checkCLIContainerNetwork) rather than on network: in general.
const noNetwork = "none"

// validateNetworkRules groups the network:-related load-time checks, mirroring
// validateImageRules.
func (c *Config) validateNetworkRules() error {
	err := c.validateNetworkValues()
	if err != nil {
		return err
	}

	// Placement before needs-image, so `network:` on a put reports the reason
	// that actually helps ("not valid on put steps") rather than the
	// needs-image rule tripping first on a step whose image this walk does not
	// resolve.
	err = c.rejectOnGetAndPut("network", func(s *Step) bool { return s.Network != "" })
	if err != nil {
		return err
	}

	return c.validateNetworkNeedsImage()
}

// validateNetworkValues rejects a network: value docker's parser would read as
// a flag. Same reasoning as checkUserValue: `--network <value>` sits before
// the "--", so nothing else protects it.
//
// The value is otherwise passed through rather than checked against a fixed
// set. "none" and "host" are the ones that matter, but docker also takes a
// named network, and a pipeline that needs to reach a service on a compose
// network has a real use for that. A typo is caught by docker itself at
// container start, surfacing like any other docker-level failure.
func (c *Config) validateNetworkValues() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		if strings.HasPrefix(settings.Network, "-") {
			return fmt.Errorf("%s: network %q must not start with '-' (docker would parse it as a flag, not a network)", context, settings.Network)
		}

		return nil
	})
}

// validateNetworkNeedsImage rejects network: without image:.
//
// Unlike env:, which means something on both paths, network: only has an
// effect on a container — a host command uses the host's network and there is
// nothing to isolate it with. Accepting `network: none` on a host command
// would quietly promise an isolation that isn't there, which is worse than
// refusing, and is the kind of mistake worth catching at load time rather
// than in an incident review.
//
// A step is checked against its RESOLVED image (visitContainerSettings
// supplies it): `network:` on a step whose image comes from the tasks:/agents:
// entry it references is perfectly valid, and the step's own image: is empty
// there.
func (c *Config) validateNetworkNeedsImage() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		if settings.Network != "" && settings.Image == "" {
			return fmt.Errorf("%s: network %q requires image: — a host-executed command uses the host's network, so this would promise an isolation it cannot provide", context, settings.Network)
		}

		return nil
	})
}
