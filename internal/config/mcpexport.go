package config

import (
	"errors"
	"fmt"
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
// Only MCP grants and sub-agents may be exported. A custom run: tool would
// put a caller's input into a shell on the daemon's own machine, and a
// built-in has no workspace to work in. Never hashed: an export changes
// nothing a build does.
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

	return validateExportedNarrowing(spec)
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
	default:
		return errors.New("a custom run: tool would put a caller's input into a shell on the daemon's machine, so it cannot be exported")
	}

	return nil
}

func validateExportedNarrowing(spec ToolSpec) error {
	switch {
	case len(spec.Args) > 0 && spec.MCPTool == "":
		return errors.New("args: pins one tool's arguments, so it needs the single-tool form {mcp, tool}")
	case spec.MaxCalls != 0:
		return errors.New("max_calls: counts within one conversation, which an export does not have; use the export's rate: instead")
	case spec.MaxOutputBytes != 0:
		return errors.New("max_output_bytes: spills to a file in a workspace, which an export does not have")
	case spec.Required:
		return errors.New("required: aborts a step, which an export does not have")
	}

	return nil
}
