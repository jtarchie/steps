package config

// The mcp_servers: entry — transport, credentials, cwd resolution — plus the
// two places a config can point at one: an agent's tool grant and a resource
// type's check/in/out backend.

import (
	"log/slog"
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

// MCPToolCall names the remote tool a resource-type lifecycle stage calls,
// and (optionally) shapes the arguments it is called with.
//
// Args is the argument object sent to Tool. Every string leaf in it is a
// template rendered over exactly the data the shell path's command for that
// same stage gets — {source} for check, {source, version, params} for in,
// {source, params} for out — so a value the remote tool requires can be
// lifted out of whichever of them holds it. Naming one a stage does not have
// (a {{ .version }} in check:) fails the step rather than rendering empty:
//
//	in:
//	  tool: slack_read_thread
//	  args:
//	    channel_id: "{{ .version.channel }}"
//	    message_ts: "{{ .version.ts }}"
//
// This exists because an MCP tool's arguments are its own published schema,
// not ours: `slack_read_thread` requires `channel_id`/`message_ts` at the top
// level and will reject anything else. Before args:, steps sent a fixed
// envelope ({"source": source}, {"source", "version"}, {"source", "params"}),
// which no third-party server has ever declared a parameter for — so the
// mcp: backend could only ever talk to a server written against steps'
// envelope. Naming the mapping is what makes an off-the-shelf server usable.
//
// When Args is omitted the arguments are the stage's natural payload, sent
// verbatim: source for check and in, params for out. That covers the common
// case where the resource's source: IS the tool's argument object (a `check`
// whose source is `{query: ...}`), and it is why `in:` needs args: whenever
// its tool wants fields that live on the VERSION rather than the source.
type MCPToolCall struct {
	Tool string         `yaml:"tool"`
	Args map[string]any `yaml:"args,omitempty"`
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
//
// ClientID/ClientSecretEnv are the oauth-only escape hatch for an
// authorization server that does not offer dynamic client registration —
// notably Slack's, which requires clients to be backed by a pre-registered
// app with a fixed ID. Setting ClientID makes `steps mcp login` skip
// registration and use those credentials directly; everything downstream
// (PKCE, code exchange, silent refresh) is unchanged. A client ID is a
// public application identifier, so it lives in YAML like an endpoint does;
// the SECRET follows the api_key_env convention and is only ever named.
// CallbackPort pins the loopback port `steps mcp login` listens on for the
// browser redirect, and exists for the same authorization servers ClientID
// does. `steps mcp login` otherwise takes an ephemeral port, which is what
// RFC 8252 §7.3 asks servers to allow for native apps — but the MCP
// authorization spec requires the opposite ("authorization servers MUST
// validate exact redirect URIs against pre-registered values"), and the
// providers that need a pre-registered client are generally the ones
// enforcing it. Against those, an ephemeral port cannot match whatever
// redirect URI was registered in a dashboard, so login fails no matter how
// the client was configured. Setting this makes the redirect URI predictable,
// so `http://127.0.0.1:<port>/callback` can be registered once.
type MCPServerAuth struct {
	Type            string   `yaml:"type"`
	APIKeyEnv       string   `yaml:"api_key_env,omitempty"`
	ClientID        string   `yaml:"client_id,omitempty"`
	ClientSecretEnv string   `yaml:"client_secret_env,omitempty"`
	CallbackPort    int      `yaml:"callback_port,omitempty"`
	Scopes          []string `yaml:"scopes,omitempty"`
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
