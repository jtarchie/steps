package venue

import "github.com/jtarchie/steps/internal/shell"

// HeldOf reports what a placed runner's worker kept of the step's outputs after its last command, each by name and digest, and the worker URL holding them so a later Pull can reach the same machine. False for a runner that is not placed, or whose worker kept nothing.
func HeldOf(r shell.Runner) (map[string]string, string, bool) {
	placed, ok := r.(interface {
		heldOutputs() (map[string]string, string, bool)
	})
	if !ok {
		return nil, "", false
	}

	return placed.heldOutputs()
}
