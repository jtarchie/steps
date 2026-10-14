package config

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MCPExport is a named bundle of this pipeline's tools that an agent
// somewhere else can connect to through the daemon, as an MCP server, under
// the name of a person who signed in. The tools run with the pipeline's own
// credentials and the caller never sees them; what narrows a tool — a fixed
// argument, a replacement description, a time limit — is the same ToolSpec an
// agent's grant uses, so a pin written for an agent means the same here.
//
// MCP grants, sub-agents and custom run: tools may be exported; a built-in has
// no workspace to work in. A custom tool runs its command with the caller's
// values quoted into it (an unquoted one is refused, see internal/agent), in a
// directory made for that one call, on the host or in the container the export
// names. Never hashed: an export changes nothing a build does.
type MCPExport struct {
	// Name is the export's address under the pipeline: /mcp/x/<pipeline>/<name>.
	Name string `yaml:"name"`
	// Access is who may connect, a rule over the signed-in person, required:
	// an export with no rule would be one anybody signed in could use.
	Access string `yaml:"access"`
	// Tools are what a connected agent may call.
	Tools []ToolSpec `yaml:"tools"`
	// Rate bounds calls per person, as "<n>/<s|m|h>"; empty is unbounded.
	Rate string `yaml:"rate,omitempty"`
	// Image runs the export's custom run: commands in a container from this image, a fresh one per command; empty runs them on the daemon's machine.
	Image string `yaml:"image,omitempty"`
	// User, Network, Privileged and Limits are the container's, as on a task, and need Image.
	User       string           `yaml:"user,omitempty"`
	Network    string           `yaml:"network,omitempty"`
	Privileged bool             `yaml:"privileged,omitempty"`
	Limits     *ContainerLimits `yaml:"container_limits,omitempty"`
}

func (e MCPExport) containerSettings() containerSettings {
	return containerSettings{Image: e.Image, User: e.User, Network: e.Network, Privileged: e.Privileged, Limits: e.Limits}
}

func (e MCPExport) checkContainerHasCommands() error {
	if !e.hasCommands() && (e.Image != "" || e.User != "" || e.Network != "" || e.Privileged || e.Limits != nil) {
		return errors.New("image/user/network/privileged/container_limits shape where a custom run: tool's command runs, and this export has none")
	}

	return nil
}

func (e MCPExport) hasCommands() bool {
	for _, spec := range e.Tools {
		if isCustomTool(spec) {
			return true
		}
	}

	return false
}

func isCustomTool(spec ToolSpec) bool {
	return spec.Builtin == "" && spec.MCP == "" && spec.Agent == "" && spec.Name != "" && spec.Run != ""
}

// ParseRate reads "<n>/<s|m|h>" into a count and the window it is counted over.
func ParseRate(rate string) (int, time.Duration, error) {
	count, unit, found := strings.Cut(rate, "/")

	n, err := strconv.Atoi(count)
	if !found || err != nil || n <= 0 {
		return 0, 0, fmt.Errorf("rate %q is <calls>/<s|m|h>, like 60/m", rate)
	}

	window, known := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour}[unit]
	if !known {
		return 0, 0, fmt.Errorf("rate %q is <calls>/<s|m|h>, like 60/m", rate)
	}

	return n, window, nil
}

//nolint:gochecknoglobals // compiled once, read-only
var exportedToolName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func (c *Config) validateMCPExports() error {
	seen := map[string]bool{}

	var errs []error

	for i := range c.MCPExports {
		export := &c.MCPExports[i]

		err := c.validateMCPExport(export)
		if err != nil {
			errs = append(errs, fmt.Errorf("mcp_exports %q: %w", export.Name, err))
		}

		if seen[export.Name] {
			errs = append(errs, fmt.Errorf("mcp_exports %q is declared twice", export.Name))
		}

		seen[export.Name] = true
	}

	return errors.Join(errs...)
}

func (c *Config) validateMCPExport(export *MCPExport) error {
	err := ValidPipelineName(export.Name)
	if err != nil {
		return fmt.Errorf("name: %w", err)
	}

	if strings.TrimSpace(export.Access) == "" {
		return errors.New("access: is required — say who may connect, or anybody signed in could")
	}

	if len(export.Tools) == 0 {
		return errors.New("tools: is empty, so there is nothing to connect to")
	}

	err = export.checkContainerHasCommands()
	if err != nil {
		return err
	}

	if export.Rate != "" {
		_, _, err = ParseRate(export.Rate)
		if err != nil {
			return err
		}
	}

	for _, spec := range export.Tools {
		err = c.validateExportedTool(spec)
		if err != nil {
			return fmt.Errorf("tool %q: %w", ToolSpecName(spec), err)
		}
	}

	return nil
}

// validateExportedTool allows what a caller somewhere else may safely reach: an MCP grant or a sub-agent, and the narrowing a call can honour without a conversation around it.
func (c *Config) validateExportedTool(spec ToolSpec) error {
	err := c.validateExportedKind(spec)
	if err != nil {
		return err
	}

	err = validateExportedNarrowing(spec)
	if err != nil {
		return err
	}

	return validateToolParamsShape("export", grantPosition, spec)
}

func (c *Config) validateExportedKind(spec ToolSpec) error {
	switch {
	case spec.MCP != "":
		_, err := c.FindMCPServer(spec.MCP)
		if err != nil {
			return err
		}
	case spec.Agent != "":
		_, err := c.FindAgent(spec.Agent)
		if err != nil {
			return err
		}
	case spec.Builtin != "":
		return errors.New("a built-in has no workspace to work in outside a step, so it cannot be exported")
	case !isCustomTool(spec):
		return errors.New("a tool is an {mcp, tool} grant, an {agent}, or a custom tool with a name and a run")
	case !exportedToolName.MatchString(spec.Name):
		return fmt.Errorf("name %q is not a tool name a caller's agent can use: letters, digits, _ and -, at most 64", spec.Name)
	}

	return nil
}

func validateExportedNarrowing(spec ToolSpec) error {
	switch {
	case len(spec.Args) > 0 && spec.MCP != "" && spec.MCPTool == "":
		return errors.New("args: pins one tool's arguments, so it needs the single-tool form {mcp, tool}")
	case len(spec.Args) > 0 && spec.Agent != "":
		return errors.New("args: pins a custom tool's or an MCP tool's arguments; a sub-agent's one argument is its request")
	case spec.MaxCalls != 0:
		return errors.New("max_calls: counts within one conversation, which an export does not have; use the export's rate: instead")
	case spec.Required:
		return errors.New("required: aborts a step, which an export does not have")
	}

	return nil
}
