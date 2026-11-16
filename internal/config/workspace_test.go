package config

import (
	"strings"
	"testing"
)

// TestWorkspaceBlockIsRefusedNamingItsReplacement: every key the block had is the operator's now, so a pipeline still carrying one is told where it went rather than "field workspace not found".
func TestWorkspaceBlockIsRefusedNamingItsReplacement(t *testing.T) {
	t.Parallel()

	for name, block := range map[string]string{
		"root":     "  root: /var/lib/steps\n",
		"cache":    "  cache:\n    resources: true\n    max_entries: 20\n",
		"strategy": "  strategy: copy\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			path := writeConfig(t, "jobs:\n- name: build\n  plan:\n  - task: build\n    run: echo hi\nworkspace:\n"+block)

			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("a pipeline with a workspace: block loaded")
			}

			for _, want := range []string{"line 6", "workspace: is no longer a pipeline key", "delete the block", "--workspace-root", "resource cache is always on", "--resource-cache-max", "strategy: was removed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
		})
	}
}
