package config

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// validateUniqueNames refuses two top-level entries of one kind under one
// name, as Concourse's set-pipeline does: every Find* returns the first
// match, so the second would silently never be used, and the store keys
// history by name, so the two would share one.
//
// It runs before built-ins are registered, because declaring a built-in's
// name is how a pipeline replaces a resource type or configures an agent.
// mcp_servers: is checked by validateMCPServers.
func (c *Config) validateUniqueNames(source []byte) error {
	var doc yaml.Node

	// The strict decode already succeeded; a failed re-parse only costs the line numbers.
	_ = yaml.Unmarshal(source, &doc)

	var root *yaml.Node
	if len(doc.Content) > 0 {
		root = doc.Content[0]
	}

	kinds := []struct {
		key   string
		names []string
	}{
		{"resource_types", names(c.ResourceTypes, func(r ResourceType) string { return r.Name })},
		{"resources", names(c.Resources, func(r Resource) string { return r.Name })},
		{"agents", names(c.Agents, func(a Agent) string { return a.Name })},
		{"tasks", names(c.Tasks, func(t Task) string { return t.Name })},
		{"jobs", names(c.Jobs, func(j Job) string { return j.Name })},
	}

	var errs []error

	for _, kind := range kinds {
		list := mappingValue(root, kind.key)
		seen := make(map[string]int, len(kind.names))

		for i, name := range kind.names {
			if name == "" {
				continue
			}

			first, dup := seen[name]
			if !dup {
				seen[name] = i

				continue
			}

			errs = append(errs, fmt.Errorf("%s and %s have the same name %q",
				entryLabel(kind.key, first, list), entryLabel(kind.key, i, list), name))
		}
	}

	return errors.Join(errs...)
}

// entryLabel renders `jobs[2] (line 9)`, dropping the line when the node tree does not reach that entry.
func entryLabel(key string, index int, list *yaml.Node) string {
	label := fmt.Sprintf("%s[%d]", key, index)

	if list != nil && list.Kind == yaml.SequenceNode && index < len(list.Content) {
		label += lineSuffix(list.Content[index].Line)
	}

	return label
}
