package config

// container_limits: and privileged: — the two execution settings that change
// what a containerized command is ALLOWED to do, rather than where it runs.

import "fmt"

// ContainerLimits caps a container's CPU and memory, mirroring Concourse's
// container_limits: (concourse-ci.org/docs/steps/task/).
//
// Both fields are optional and independent: setting one and not the other is
// the common case, since memory is the limit people actually reach for.
type ContainerLimits struct {
	// CPU is docker's `--cpu-shares`: a RELATIVE weight against other
	// containers, not a core count and not a percentage. 1024 is the default
	// weight, so 512 is "half the share of a default container when both are
	// contending" — and it caps nothing at all on an idle machine. Named cpu:
	// to match Concourse rather than renamed to something more honest, since a
	// pipeline moving between the two should mean the same thing in both.
	CPU int `yaml:"cpu,omitempty"`
	// Memory is docker's `--memory`, in BYTES. A container exceeding it is
	// OOM-killed by the kernel, which surfaces as exit code 137 — worth
	// knowing, because that reads as an ordinary command failure and not as a
	// limit being enforced.
	Memory int64 `yaml:"memory,omitempty"`
}

// set reports whether l asks for anything.
func (l *ContainerLimits) set() bool {
	return l != nil && (l.CPU > 0 || l.Memory > 0)
}

// validateLimitsRules groups the container_limits:/privileged: load-time
// checks, mirroring validateNetworkRules.
func (c *Config) validateLimitsRules() error {
	err := c.validateLimitValues()
	if err != nil {
		return err
	}

	// Placement before needs-image, for the reason network: does it: a
	// privileged: on a put should report "not valid on put steps" rather than
	// the needs-image rule tripping first on a step whose image this walk
	// cannot resolve.
	err = c.rejectOnGetAndPut("privileged", func(s *Step) bool { return s.Privileged })
	if err != nil {
		return err
	}

	err = c.rejectOnGetAndPut("container_limits", func(s *Step) bool { return s.Limits != nil })
	if err != nil {
		return err
	}

	return c.validateLimitsNeedImage()
}

// validateLimitValues rejects a limit that is present but not a limit.
//
// A negative or zero value written explicitly is a mistake worth catching: it
// reads as "no limit" to docker, so the pipeline would claim a cap it does not
// have. Omitting the field is how you say "unlimited".
func (c *Config) validateLimitValues() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		if settings.Limits == nil {
			return nil
		}

		if settings.Limits.CPU < 0 {
			return fmt.Errorf("%s: container_limits.cpu must be a positive share weight (omit it for no limit)", context)
		}

		if settings.Limits.Memory < 0 {
			return fmt.Errorf("%s: container_limits.memory must be a positive number of bytes (omit it for no limit)", context)
		}

		if !settings.Limits.set() {
			return fmt.Errorf("%s: container_limits sets neither cpu nor memory, so it caps nothing — omit it, or give it a limit", context)
		}

		return nil
	})
}

// validateLimitsNeedImage rejects either setting without image:, for the
// reason network: is rejected.
//
// Both describe a CONTAINER. A host-executed command has no cgroup to cap and
// no privilege to raise, so accepting either there would promise a limit (or a
// capability) that is not applied — and a limit believed to be in force is
// worse than one visibly absent.
func (c *Config) validateLimitsNeedImage() error {
	return c.visitContainerSettings(func(context string, settings containerSettings) error {
		if settings.Image != "" {
			return nil
		}

		if settings.Privileged {
			return fmt.Errorf("%s: privileged requires image: — a host-executed command already runs with whatever privileges started steps, so this would promise an elevation it does not perform", context)
		}

		if settings.Limits != nil {
			return fmt.Errorf("%s: container_limits requires image: — a host-executed command has no container to cap, so this would promise a limit it cannot enforce", context)
		}

		return nil
	})
}
