package config

// The extra_hosts: field: names a container resolves through its own /etc/hosts (docker's --add-host).

import (
	"fmt"
	"net"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// hostGateway is docker's alias for the machine running the daemon, which it substitutes when the container is created.
const hostGateway = "host-gateway"

// ExtraHosts are `name:address` entries, written as a map (`name: address`) or a list of `name:address` strings. A map is stored in sorted name order and a list in declared order, so the same YAML always yields the same cache key.
type ExtraHosts []string

// UnmarshalYAML takes a mapping or a sequence. A mapping's keys are host names, not field names, so rejectUnknownKeys has nothing to check. An empty map or list decodes to nil, as unset does: sameContainer compares with DeepEqual, where nil and empty differ.
func (h *ExtraHosts) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.MappingNode:
		var entries map[string]string

		err := node.Decode(&entries)
		if err != nil {
			return fmt.Errorf("extra_hosts: each value is an address or %s: %w", hostGateway, err)
		}

		names := make([]string, 0, len(entries))
		for name := range entries {
			names = append(names, name)
		}

		slices.Sort(names)

		var hosts ExtraHosts
		for _, name := range names {
			hosts = append(hosts, name+":"+entries[name])
		}

		*h = hosts

		return nil
	case yaml.SequenceNode:
		var entries []string

		err := node.Decode(&entries)
		if err != nil {
			return fmt.Errorf("extra_hosts: each entry is a name:address string: %w", err)
		}

		if len(entries) == 0 {
			entries = nil
		}

		*h = entries

		return nil
	case yaml.DocumentNode, yaml.ScalarNode, yaml.AliasNode:
	}

	return fmt.Errorf("line %d: extra_hosts is a map of name: address or a list of name:address strings", node.Line)
}

// validateExtraHostsRules refuses an extra_hosts: entry that is not name:address, one on an entry with no image to apply it to, and one beside network: none, which leaves nothing for the name to reach.
func (c *Config) validateExtraHostsRules() error {
	err := c.visitContainerSettings(func(context string, settings containerSettings) error {
		if len(settings.ExtraHosts) == 0 {
			return nil
		}

		for _, entry := range settings.ExtraHosts {
			err := validateExtraHost(entry)
			if err != nil {
				return fmt.Errorf("%s: %w", context, err)
			}
		}

		if settings.Image == "" {
			return fmt.Errorf("%s: extra_hosts requires image: — a host-executed command reads the host's own /etc/hosts, so there is no container to add the names to", context)
		}

		if settings.Network == "none" {
			return fmt.Errorf("%s: extra_hosts beside network: none — the container has no network to reach the names through", context)
		}

		return nil
	})
	if err != nil {
		return err
	}

	return c.validateExtraHostsStepNetwork()
}

// validateExtraHostsStepNetwork refuses a step that sets network: none over a tasks: or agents: entry carrying extra_hosts: the step's own settings never hold the entry's names, so the walk above cannot see the pair.
func (c *Config) validateExtraHostsStepNetwork() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Network != "none" || len(c.stepExtraHosts(*step)) == 0 {
				return nil
			}

			return fmt.Errorf("%s: network: none over an entry with extra_hosts — the container has no network to reach the names through", label)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// stepExtraHosts is the extra_hosts of the tasks: or agents: entry step references, nil for anything else.
func (c *Config) stepExtraHosts(step Step) ExtraHosts {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		if task := c.taskEntry(step); task != nil {
			return task.ExtraHosts
		}
	case StepKindAgent:
		agent, err := c.FindAgent(step.Agent)
		if err == nil {
			return agent.ExtraHosts
		}
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble,
		StepKindLoadVar, StepKindApproval, StepKindAsk, StepKindDo:
		// Only a task or agent step references an entry that can carry extra_hosts; a get's or put's resource type is checked as its own entry.
	}

	return nil
}

// validateExtraHost checks one entry's shape: a name, a colon, and an IP address or host-gateway.
func validateExtraHost(entry string) error {
	name, address, ok := strings.Cut(entry, ":")
	if !ok || name == "" || address == "" {
		return fmt.Errorf("extra_hosts entry %q must be name:address", entry)
	}

	if address != hostGateway && net.ParseIP(address) == nil {
		return fmt.Errorf("extra_hosts entry %q: %q is neither an IP address nor %s", entry, address, hostGateway)
	}

	return nil
}
