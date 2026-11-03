package pipeline

import (
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// withIsolation sets the container-shaping fields of spec from iso: the one place a task's or an agent's settings are copied onto a RunnerSpec, since shell is stdlib-only and cannot take config.Isolation itself.
func withIsolation(spec shell.RunnerSpec, iso config.Isolation) shell.RunnerSpec {
	spec.User, spec.Network, spec.Privileged, spec.Sandbox = iso.User, iso.Network, iso.Privileged, iso.Sandbox
	spec.CPUShares, spec.MemoryBytes = iso.Limits.CPUShares(), iso.Limits.MemoryBytes()

	return spec
}
