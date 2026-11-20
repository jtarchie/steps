package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// The daemon's fence: policy (jtarchie/steps#263). Whether the OS sandbox exists is a fact about the machine that runs the commands, which is why this is judged here and not by the file validator.
const (
	fenceOff     = "off"
	fenceWarn    = "warn"
	fenceEnforce = "enforce"
)

// errUnfenced marks a refusal by fence: enforce, which a restart reads as the daemon's policy rather than the file's fault.
var errUnfenced = errors.New("runs commands unfenced, and fence: enforce accepts none")

// fencePolicy is what judges a pipeline: the word, and under enforce the daemon file's half of each opt-out.
type fencePolicy struct {
	mode string
	// allow is allow-unfenced:, pipeline name to the entities it may run unfenced.
	allow map[string][]string
}

// checkFence refuses any word but the three this daemon does: an operator who mistyped enforce believes the daemon refuses what it would run.
func checkFence(value string) error {
	switch value {
	case "", fenceOff, fenceWarn, fenceEnforce:
		return nil
	default:
		return fmt.Errorf("fence: %q is not a policy; want off, warn or enforce", value)
	}
}

// parseAllowUnfenced holds allow-unfenced: to a map of pipeline names to lists of "<kind> <name>", so a mistyped entry is refused rather than read as granting nothing.
func parseAllowUnfenced(raw any) (map[string][]string, error) {
	const shape = "allow-unfenced: must map each pipeline name to a list of entities, e.g. {app: [task post]}"

	pipelines, ok := raw.(map[string]any)
	if !ok {
		return nil, errors.New(shape)
	}

	allow := map[string][]string{}

	for name, value := range pipelines {
		list, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s; %q is not a list", shape, name)
		}

		for _, item := range list {
			entity, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s; %q holds %v", shape, name, item)
			}

			kind, rest, _ := strings.Cut(entity, " ")
			if !slices.Contains(config.EntityKinds(), kind) || strings.TrimSpace(rest) == "" {
				return nil, fmt.Errorf("allow-unfenced: %s: %q is not an entity; want one of %s followed by its name, as fence: warn lists it", name, entity, strings.Join(config.EntityKinds(), ", "))
			}

			allow[name] = append(allow[name], entity)
		}
	}

	return allow, nil
}

// judge is what the policy makes of cfg, set as name, on this machine: the lines it lists, and under enforce the refusal of every entity both keys do not opt out.
func (p fencePolicy) judge(name string, cfg *config.Config) ([]string, error) {
	if p.mode != fenceWarn && p.mode != fenceEnforce {
		return nil, nil
	}

	var listed, refused []string

	for _, host := range cfg.Unfenced(shell.SandboxAvailable() == nil) {
		line := fmt.Sprintf("%s: %s [%s", host.Where, host.Why, host.Entity)
		if host.Reason != "" {
			line += fmt.Sprintf(", unfenced: %q", host.Reason)
		}

		line += "]"

		if p.mode == fenceWarn {
			listed = append(listed, line)

			continue
		}

		switch {
		case host.Reason == "":
			refused = append(refused, line+" — the pipeline declares no unfenced: on it")
		case !slices.Contains(p.allow[name], host.Entity):
			refused = append(refused, fmt.Sprintf("%s — allow-unfenced: does not name %q for %s", line, host.Entity, name))
		default:
			listed = append(listed, line)
		}
	}

	if len(refused) > 0 {
		return nil, fmt.Errorf("%s %w; fence each as its line says, or opt it out with BOTH unfenced: \"<reason>\" on it in the pipeline and its [entity] under allow-unfenced: %s in the daemon's config:\n  %s", name, errUnfenced, name, strings.Join(refused, "\n  "))
	}

	return listed, nil
}

// unfenced is what the policy lists for a configuration accept already passed, so judge has nothing left to refuse.
func (d *daemon) unfenced(name string, cfg *config.Config) []string {
	lines, _ := d.fence.judge(name, cfg)

	return lines
}

// sayUnfenced is the listing set and validate print, one place per line: under enforce, every one is opted out.
func sayUnfenced(out io.Writer, lines []string) {
	if len(lines) == 0 {
		return
	}

	sayf(out, "warning: fence: %d command(s) run unfenced:\n", len(lines))

	for _, line := range lines {
		sayf(out, "  %s\n", line)
	}
}
