package cli

import (
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/jtarchie/steps/internal/config"
)

// The daemon's fence: policy (jtarchie/steps#263). Whether the OS sandbox exists is a fact about the machine that runs the commands, which is why this is judged here and not by the file validator.
const (
	fenceOff  = "off"
	fenceWarn = "warn"
)

// checkFence refuses any word but the two this daemon does, enforce included: an operator who wrote enforce believes the daemon refuses what it would only list.
func checkFence(value string) error {
	switch value {
	case "", fenceOff, fenceWarn:
		return nil
	case "enforce":
		return errors.New("fence: enforce is not available yet (jtarchie/steps#263); use warn, which accepts and lists every unfenced command")
	default:
		return fmt.Errorf("fence: %q is not a policy; want off or warn", value)
	}
}

// sayUnfenced is the listing set and validate print, one place per line under a heading that names the policy asking.
func sayUnfenced(out io.Writer, lines []string) {
	if len(lines) == 0 {
		return
	}

	sayf(out, "warning: fence: warn — %d command(s) run unfenced:\n", len(lines))

	for _, line := range lines {
		sayf(out, "  %s\n", line)
	}
}

// unfencedLines is what the policy lists for cfg on this machine: nothing under off.
func unfencedLines(policy string, cfg *config.Config) []string {
	if policy != fenceWarn {
		return nil
	}

	var lines []string

	for _, host := range cfg.Unfenced(runtime.GOOS == "darwin") {
		lines = append(lines, host.Where+": "+host.Why)
	}

	return lines
}
