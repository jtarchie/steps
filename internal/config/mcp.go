package config

import (
	"fmt"
	"path/filepath"
)

// validateMCPToolGrants enforces the MCP tool grant rules at load time,
// mirroring validateAgentGraph's sub-agent tool rules: an MCP grant's shape
// (validateMCPToolShape) on every agents: entry and fix: tool list — both
// the top-level tasks: fix and any step-level fix: override, since RunFix
// funnels fix.Tools through the same resolveEffectiveTools merge a normal
// agent step's tools: does (see internal/agent/fix.go) — and that MCP tools
// are granted rather than added inline on a step.
func (c *Config) validateMCPToolGrants() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		for _, tool := range agent.Tools {
			if tool.MCP == "" {
				continue
			}

			err := c.validateMCPToolShape(fmt.Sprintf("agent %q", agent.Name), tool)
			if err != nil {
				return err
			}
		}
	}

	for i := range c.Tasks {
		err := c.checkFixMCPToolShapes(fmt.Sprintf("task %q fix", c.Tasks[i].Name), c.Tasks[i].Fix)
		if err != nil {
			return err
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return c.checkFixMCPToolShapes(label+" fix", step.Fix)
		})
		if err != nil {
			return err
		}
	}

	return c.rejectInlineMCPTools()
}

// checkFixMCPToolShapes validates the shape of every MCP tool grant in a
// fix:'s own tools: override, if any.
func (c *Config) checkFixMCPToolShapes(context string, fix *FixSpec) error {
	if fix == nil {
		return nil
	}

	for _, tool := range fix.Tools {
		if tool.MCP == "" {
			continue
		}

		err := c.validateMCPToolShape(context, tool)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateMCPToolShape checks one MCP tool grant entry: it must set no
// custom-tool/sub-agent fields, must reference a configured mcp_servers:
// entry, must not set both tool: and tools:, must not set args: (an MCP
// tool's arguments are schema-shaped by the remote server, not a flat
// string template), and may only set description:/required:/max_calls:
// (single-tool concepts) when tool: selects exactly one remote tool — not
// on the named-subset (tools:) or bare "grant everything" form.
func (c *Config) validateMCPToolShape(context string, tool ToolSpec) error {
	err := validateMCPToolFields(context, tool)
	if err != nil {
		return err
	}

	_, err = c.FindMCPServer(tool.MCP)
	if err != nil {
		return fmt.Errorf("%s: mcp tool: %w", context, err)
	}

	return nil
}

// setsCustomOrSubAgentFields reports whether tool sets any field that only
// makes sense on a custom or sub-agent tool — never valid alongside mcp:.
func setsCustomOrSubAgentFields(tool ToolSpec) bool {
	return tool.Builtin != "" || tool.Name != "" || tool.Run != "" || tool.Agent != ""
}

// validateMCPToolFields checks the field-shape rules of one MCP tool grant
// entry, without resolving MCP against the server list (see
// validateMCPToolShape, its only caller) — split out to keep that function's
// branch count down (cyclop).
func validateMCPToolFields(context string, tool ToolSpec) error {
	if setsCustomOrSubAgentFields(tool) {
		return fmt.Errorf("%s: mcp tool %q must not also set builtin/name/run/agent", context, tool.MCP)
	}

	if tool.MCPTool != "" && len(tool.MCPTools) > 0 {
		return fmt.Errorf("%s: mcp tool %q: tool and tools are mutually exclusive", context, tool.MCP)
	}

	if tool.MCPTool == "" && (tool.Description != "" || tool.Required || tool.MaxCalls != 0) {
		return fmt.Errorf("%s: mcp tool %q: description/required/max_calls are only valid when tool: selects a single remote tool", context, tool.MCP)
	}

	return validateMCPToolGuards(context, tool)
}

// validateMCPToolGuards checks the args:/max_calls: guard fields — split out
// of validateMCPToolFields to keep its branch count down (cyclop).
func validateMCPToolGuards(context string, tool ToolSpec) error {
	if tool.Args != nil {
		return fmt.Errorf("%s: mcp tool %q: args is not valid on mcp tools (arguments are schema-shaped by the remote server)", context, tool.MCP)
	}

	if tool.MaxCalls < 0 {
		return fmt.Errorf("%s: mcp tool %q: max_calls must be >= 0", context, tool.MCP)
	}

	return nil
}

// rejectInlineMCPTools rejects an MCP tool grant added inline on a step (or
// a step hook). An MCP grant is a capability grant, like a sub-agent: it
// must be declared on the agents: entry (or a fix:'s own tools:, resolved
// via the same agent-grant merge — see checkFixMCPToolShapes above) and
// selected by bare name, not introduced on the step.
func (c *Config) rejectInlineMCPTools() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			for _, tool := range step.Tools {
				if tool.MCP != "" {
					return fmt.Errorf("%s: mcp tool %q must be granted on an agent, not added inline on a step", label, tool.MCP)
				}
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateResourceTypeConfig checks every resource_types: entry's config:
// shape: the shell (check/in/out strings) and mcp: forms are mutually
// exclusive, an mcp: block requires check: and a resolvable server, and a
// put step may only target an mcp-backed resource type that declares out:
// (there is no equivalent of an empty shell out: silently no-op-ing — an
// mcp-backed put with no configured tool is rejected at load rather than
// failing confusingly at run time).
func (c *Config) validateResourceTypeConfig() error {
	for i := range c.ResourceTypes {
		err := c.validateOneResourceTypeConfig(c.ResourceTypes[i])
		if err != nil {
			return err
		}
	}

	return c.validateMCPResourcePuts()
}

// validateOneResourceTypeConfig checks a single resource_types: entry's
// config: shape — split out of validateResourceTypeConfig to keep that
// function's branch count down (cyclop).
func (c *Config) validateOneResourceTypeConfig(rt ResourceType) error {
	hasShell := rt.Config.Check != "" || rt.Config.In != "" || rt.Config.Out != ""
	hasMCP := rt.Config.MCP != nil

	if hasShell && hasMCP {
		return fmt.Errorf("resource_type %q: check/in/out and mcp: are mutually exclusive", rt.Name)
	}

	if !hasMCP {
		return nil
	}

	return c.validateMCPResourceConfig(rt.Name, rt.Config.MCP)
}

// validateMCPResourceConfig checks one resource type's mcp: block — split
// out of validateOneResourceTypeConfig to keep that function's branch count
// down (cyclop).
func (c *Config) validateMCPResourceConfig(rtName string, mcp *MCPResourceConfig) error {
	if mcp.Check == nil || mcp.Check.Tool == "" {
		return fmt.Errorf("resource_type %q: mcp.check.tool is required", rtName)
	}

	if mcp.In != nil && mcp.In.Tool == "" {
		return fmt.Errorf("resource_type %q: mcp.in.tool must not be empty when mcp.in is set", rtName)
	}

	if mcp.Out != nil && mcp.Out.Tool == "" {
		return fmt.Errorf("resource_type %q: mcp.out.tool must not be empty when mcp.out is set", rtName)
	}

	srv, err := c.FindMCPServer(mcp.Server)
	if err != nil {
		return fmt.Errorf("resource_type %q: %w", rtName, err)
	}

	// A relative cwd: is resolved against an agent step's working directory
	// (see MCPServer.Cwd / WithResolvedMCPCwd). A resource type's
	// check/in/out has no such step, so the path would silently resolve
	// against whatever directory steps itself was invoked from — reject it
	// at load rather than let it half-work.
	if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
		return fmt.Errorf(
			"resource_type %q: mcp server %q has a relative cwd %q, which resolves against an agent step's working directory; a resource type has no step workspace, so its server needs an absolute cwd",
			rtName, srv.Name, srv.Cwd,
		)
	}

	return nil
}

// validateMCPResourcePuts rejects a put step targeting an mcp-backed
// resource type whose config sets no out: tool — the mcp analogue of a
// shell resource type with an empty out: command, but checkable (and
// checked) at load time instead of failing confusingly at run time.
func (c *Config) validateMCPResourcePuts() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Put == "" {
				return nil
			}

			resource, err := c.FindResource(step.Put)
			if err != nil {
				return nil //nolint:nilerr // unresolvable put target is caught elsewhere at run time
			}

			resourceType, err := c.FindResourceType(resource.Type)
			if err != nil {
				return nil //nolint:nilerr // unresolvable resource type is caught elsewhere at run time
			}

			if resourceType.Config.MCP != nil && resourceType.Config.MCP.Out == nil {
				return fmt.Errorf("%s: put %q targets mcp-backed resource type %q, which sets no mcp.out.tool; add one, or respond via an agent step granted the server's tools instead", label, step.Put, resourceType.Name)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// validateMCPServerTransport checks that srv sets exactly one of
// endpoint (http)/command (stdio) and that the fields belonging to the
// other transport are unset.
func validateMCPServerTransport(srv MCPServer) error {
	switch {
	case srv.Endpoint == "" && srv.Command == "":
		return fmt.Errorf("mcp server %q: one of endpoint (http) or command (stdio) is required", srv.Name)
	case srv.Endpoint != "" && srv.Command != "":
		return fmt.Errorf("mcp server %q: endpoint and command are mutually exclusive (http vs stdio transport)", srv.Name)
	case srv.Command == "":
		return validateMCPServerHTTPOnlyFields(srv)
	default:
		return validateMCPServerStdioAuth(srv)
	}
}

// validateMCPServerHTTPOnlyFields rejects args/cwd (stdio-only fields) on
// an http server.
func validateMCPServerHTTPOnlyFields(srv MCPServer) error {
	if len(srv.Args) != 0 || srv.Cwd != "" {
		return fmt.Errorf("mcp server %q: args/cwd are only valid with command (stdio transport)", srv.Name)
	}

	return nil
}

// validateMCPServerStdioAuth rejects any auth: on a stdio server — it has
// no HTTP request to attach a bearer token to, and oauthTokenSource pins a
// persisted token to srv.Endpoint, which an empty endpoint would make
// vacuous.
func validateMCPServerStdioAuth(srv MCPServer) error {
	if srv.Auth.Type != "" && srv.Auth.Type != "none" {
		return fmt.Errorf("mcp server %q: auth.type %q requires an http endpoint; a stdio server has no request to authenticate", srv.Name, srv.Auth.Type)
	}

	return nil
}
