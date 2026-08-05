package config

// serial: / serial_groups: — stop jobs racing each other.

import "fmt"

// SerialGroupsByJob maps each job to the serial groups it belongs to.
//
// `serial: true` contributes an implicit group named after the job itself.
// That is a statement of intent rather than a switch: this runner ALWAYS
// serializes builds of one job (see Store.ClaimNextJob), so serial: true
// documents a guarantee the pipeline is relying on rather than turning one on.
func (c *Config) SerialGroupsByJob() map[string][]string {
	groups := map[string][]string{}

	for _, job := range c.Jobs {
		var names []string

		if job.Serial {
			names = append(names, "job:"+job.Name)
		}

		names = append(names, job.SerialGroups...)

		if len(names) > 0 {
			groups[job.Name] = names
		}
	}

	return groups
}

// validateSerial rejects the two spellings that would promise something this
// runner does not do.
func (c *Config) validateSerial() error {
	for _, job := range c.Jobs {
		seen := map[string]bool{}

		for _, group := range job.SerialGroups {
			if group == "" {
				return fmt.Errorf("job %q: serial_groups must not contain an empty name", job.Name)
			}

			if seen[group] {
				return fmt.Errorf("job %q: serial_groups names %q twice", job.Name, group)
			}

			seen[group] = true
		}
	}

	return nil
}
