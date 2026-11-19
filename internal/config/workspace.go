package config

// workspace: left the pipeline file: where a machine keeps its build trees is the operator's call (steps run/web --workspace-root), not the author's.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// refuseWorkspaceBlock names what each workspace: key became, before the strict decode would answer only "field workspace not found". Refused rather than kept as an alias: an alias honored keeps the pipeline overriding the operator, the thing being removed, and one ignored reads as configured and binds nothing.
func refuseWorkspaceBlock(doc *yaml.Node) error {
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	top := doc.Content[0].Content

	for i := 0; i+1 < len(top); i += 2 {
		if top[i].Value == "workspace" {
			return workspaceMoved(top[i].Line)
		}
	}

	return nil
}

func workspaceMoved(line int) error {
	return fmt.Errorf("line %d: workspace: is no longer a pipeline key; delete the block. "+
		"root: is the operator's `--workspace-root` on steps run and steps web (a daemon defaults to workspace/ beside its state database); "+
		"cache.resources: is gone, as the resource cache is always on under a durable root, as Concourse's is; "+
		"cache.max_entries: is the operator's `--resource-cache-max`; "+
		"strategy: was removed and needs no replacement, copy being the only one", line)
}
