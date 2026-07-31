package config

// The mcp_servers: entry — transport, credentials, cwd resolution — plus the
// two places a config can point at one: an agent's tool grant and a resource
// type's check/in/out backend.

import (
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
)

// MCPResourceConfig backs a resource type's check/in/out with calls to a
// named mcp_servers: entry instead of shell commands. Check is required (a
// type with no way to discover versions is useless); In and Out are
// optional — see resource.CheckVersions/RunIn/RunOut's mcp*/ branches for
// exactly what arguments each call receives and how its result is used.
type MCPResourceConfig struct {
	Server string       `yaml:"server"`
	Check  *MCPToolCall `yaml:"check,omitempty"`
	In     *MCPToolCall `yaml:"in,omitempty"`
	Out    *MCPToolCall `yaml:"out,omitempty"`
}

// MCPToolCall names the remote tool a resource-type lifecycle stage calls.
type MCPToolCall struct {
	Tool string `yaml:"tool"`
}

// MCPServer is a reusable, named MCP server connection: configured once
// under mcp_servers: and shared across any number of agents: tool grants
// and resource_types: mcp: backends — the same once-configured/many-
// consumers idiom as Agent/Resource. Two transports are supported: HTTP
// (Streamable HTTP, via Endpoint) or stdio (a local subprocess, via Command/
// Args/Cwd) — exactly one of Endpoint/Command is set (see
// validateMCPServerTransport). A stdio server has no request to attach
// credentials to, so Auth must be unset ("none") when Command is set.
type MCPServer struct {
	Name     string   `yaml:"name"`
	Endpoint string   `yaml:"endpoint,omitempty"`
	Command  string   `yaml:"command,omitempty"`
	Args     []string `yaml:"args,omitempty"`
	// Cwd is the working directory a stdio server's subprocess is spawned
	// in. An ABSOLUTE path is used verbatim — a fixed location on the host,
	// resolved identically for every step. A RELATIVE path is resolved
	// against the agent step's own working directory (see
	// WithResolvedMCPCwd), which is what lets a server be pointed at an
	// input artifact — `cwd: repo` for a language server that must index the
	// same materialized tree the agent's file tools read. Empty inherits the
	// steps process's own cwd.
	//
	// Relative only makes sense where a step workspace exists, so it is
	// rejected for a server backing a resource type's mcp: config — a
	// check/in/out runs with no agent step to resolve against.
	Cwd  string        `yaml:"cwd,omitempty"`
	Auth MCPServerAuth `yaml:"auth,omitempty"`
}

// WithResolvedMCPCwd returns cfg with every stdio server's relative Cwd
// joined against baseDir — the working directory of the agent step whose
// tools are about to be built. cfg is returned unchanged when no server
// needs it, so the overwhelmingly common case (no mcp_servers:, or all
// absolute) allocates nothing and hashes identically.
//
// The copy is shallow but the MCPServers slice is fresh, so a step
// resolving its own view can never mutate the shared config another step
// (with a different working directory) will resolve later.
func WithResolvedMCPCwd(cfg *Config, baseDir string) *Config {
	if cfg == nil || baseDir == "" {
		return cfg
	}

	needed := false

	for _, srv := range cfg.MCPServers {
		if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
			needed = true

			break
		}
	}

	if !needed {
		return cfg
	}

	servers := make([]MCPServer, len(cfg.MCPServers))
	copy(servers, cfg.MCPServers)

	for i, srv := range servers {
		if srv.Cwd != "" && !filepath.IsAbs(srv.Cwd) {
			servers[i].Cwd = filepath.Join(baseDir, srv.Cwd)
		}
	}

	resolved := *cfg
	resolved.MCPServers = servers

	return &resolved
}

// IsStdio reports whether srv is a stdio (local subprocess) server rather
// than an HTTP one. Exactly one of Endpoint/Command is set — see
// validateMCPServerTransport.
func (s MCPServer) IsStdio() bool {
	return s.Command != ""
}

// MCPServerAuth selects how steps authenticates to an MCP server. Type is
// "none" (default, when Auth is omitted entirely), "bearer" (a static token
// read from an OS environment variable named by APIKeyEnv — mirrors
// AgentSource.APIKeyEnv exactly: the credential is never stored in YAML),
// or "oauth" (interactive authorization-code + PKCE via `steps mcp login`,
// with silent refresh at run/watch time — see internal/mcp).
type MCPServerAuth struct {
	Type      string   `yaml:"type"`
	APIKeyEnv string   `yaml:"api_key_env,omitempty"`
	Scopes    []string `yaml:"scopes,omitempty"`
}

// FindMCPServer returns the mcp_servers: entry with the given name, or an
// error if not found.
func (c *Config) FindMCPServer(name string) (*MCPServer, error) {
	slog.Debug("mcp_server.find", "name", name)

	for i := range c.MCPServers {
		if c.MCPServers[i].Name == name {
			slog.Debug("mcp_server.find", "name", name, "found", true)

			return &c.MCPServers[i], nil
		}
	}

	return nil, notFound("mcp_servers entry", name, names(c.MCPServers, func(s MCPServer) string { return s.Name }))
}

// validateMCPServers checks every mcp_servers: entry at load time: a
// non-empty, unique name; a transport shape consistent with exactly one of
// endpoint (http)/command (stdio) — see validateMCPServerTransport; for an
// http server, an endpoint that doesn't embed userinfo (same check and
// reasoning as validateAgentEndpoints — the endpoint is merkle-hashed, so it
// must not carry a credential); and an auth block shape consistent with its
// type ("" / "none" / "bearer" / "oauth" — "bearer" requires api_key_env,
// any other type must not set it).
func (c *Config) validateMCPServers() error {
	seen := make(map[string]bool, len(c.MCPServers))

	for i := range c.MCPServers {
		srv := c.MCPServers[i]

		if srv.Name == "" {
			return fmt.Errorf("mcp_servers[%d]: name is required", i)
		}

		if seen[srv.Name] {
			return fmt.Errorf("mcp_servers: name %q is declared more than once", srv.Name)
		}

		seen[srv.Name] = true

		err := validateMCPServerTransport(srv)
		if err != nil {
			return err
		}

		if srv.Endpoint != "" {
			parsed, parseErr := url.Parse(srv.Endpoint)
			if parseErr == nil && parsed.User != nil {
				return fmt.Errorf("mcp server %q: endpoint must not embed credentials (userinfo); use auth.api_key_env instead", srv.Name)
			}
		}

		err = validateMCPServerAuth(srv)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateMCPServerAuth checks one server's auth: block shape.
func validateMCPServerAuth(srv MCPServer) error {
	switch srv.Auth.Type {
	case "", "none":
		if srv.Auth.APIKeyEnv != "" {
			return fmt.Errorf("mcp server %q: api_key_env is only valid with auth.type: bearer", srv.Name)
		}
	case "bearer":
		if srv.Auth.APIKeyEnv == "" {
			return fmt.Errorf("mcp server %q: auth.type: bearer requires api_key_env", srv.Name)
		}
	case "oauth":
		if srv.Auth.APIKeyEnv != "" {
			return fmt.Errorf("mcp server %q: api_key_env is only valid with auth.type: bearer", srv.Name)
		}
	default:
		return fmt.Errorf("mcp server %q: auth.type must be one of none, bearer, oauth (got %q)", srv.Name, srv.Auth.Type)
	}

	return nil
}

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

	// Deliberately allowed on all three grant forms, unlike
	// description/required/max_calls above, which are single-tool: only. The
	// tool worth capping is typically one noisy member of a tools: [...]
	// subset, and forcing it into its own `- mcp: server` entry to carry the
	// cap would open a second connection to the same server (buildMCPTools
	// connects once per spec).
	if tool.MaxOutputBytes < 0 {
		return fmt.Errorf("%s: mcp tool %q: max_output_bytes must be >= 0", context, tool.MCP)
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

// validateMCPResourcePuts rejects a put step whose resource type declares no
// way to put — see validateResourcePut for the rule, which covers both the
// mcp-backed and shell-backed forms.
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

			return validateResourcePut(label, step.Put, resourceType)
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
