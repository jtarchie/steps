package pipeline

// Unpacking helpers for container_limits:.

import "github.com/jtarchie/steps/internal/config"

// cpuShares/memoryBytes unpack an optional *config.ContainerLimits into the
// zero-means-omit form RunnerSpec takes, so every construction site reads the
// same and a nil limits block needs no branch at the call.
func cpuShares(l *config.ContainerLimits) int {
	if l == nil {
		return 0
	}

	return l.CPU
}

func memoryBytes(l *config.ContainerLimits) int64 {
	if l == nil {
		return 0
	}

	return l.Memory
}
