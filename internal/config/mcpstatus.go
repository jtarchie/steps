package config

// How an mcp_servers: entry is DESCRIBED, and what can be said about it without asking it anything. Both front ends render this — `steps mcp list` as a table, the web UI's mcp tab as a page — and it lives here because it used to live in internal/cli, which internal/web cannot import: two front ends over one concept had no way to share a vocabulary, so they would have drifted into two.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MCPReadiness is what is knowable about a server with no request made, and the reason a status cell says what it says.
type MCPReadiness int

const (
	// MCPReady is every static condition met: a stdio command found on PATH, a bearer variable set, or an unauthenticated endpoint, which has nothing to check.
	MCPReady MCPReadiness = iota
	// MCPMissing is a static condition that FAILS, and that waiting will not fix: the command is not on PATH, or the variable is not set.
	MCPMissing
	// MCPUnknown is a server whose readiness this package cannot answer — an oauth server's credential is a token file internal/mcp owns, and a stdio server with a relative cwd: resolves per step.
	MCPUnknown
)

// MCPStatus is one server's static status: whether it is usable, and the phrase both front ends print for it.
type MCPStatus struct {
	Readiness MCPReadiness
	// Detail is the whole cell, written for somebody who has to act on it — `$GITHUB_PAT is not set`, not `false`.
	Detail string
}

// Transport names which of the two shapes this server is.
func (s MCPServer) Transport() string {
	if s.IsStdio() {
		return "stdio"
	}

	return "http"
}

// Target renders what the server actually is: the endpoint for HTTP, the argv (plus any pinned working directory) for stdio.
func (s MCPServer) Target() string {
	if !s.IsStdio() {
		return s.Endpoint
	}

	target := strings.Join(append([]string{s.Command}, s.Args...), " ")
	if s.Cwd != "" {
		target += fmt.Sprintf(" (cwd: %s)", s.Cwd)
	}

	return target
}

// AuthLabel names the auth type and, for bearer, the environment variable the credential is read from — the thing to go check when it is the credential that is missing. Never the value.
func (s MCPServer) AuthLabel() string {
	if s.Auth.Type == "" || s.Auth.Type == "none" {
		return "none"
	}

	if s.Auth.APIKeyEnv != "" {
		return s.Auth.Type + " $" + s.Auth.APIKeyEnv
	}

	return s.Auth.Type
}

// NotProbableHere reports the status for a server that cannot honestly be probed from outside a run, or "" for one that can. A relative cwd: is resolved against the working directory of the agent step whose tools are being built (WithResolvedMCPCwd) — a build workspace that exists only during a run, so spawning it from wherever the operator happens to be would chdir somewhere else entirely and report a server that works perfectly in a run as broken.
func (s MCPServer) NotProbableHere() string {
	if s.IsStdio() && s.Cwd != "" && !filepath.IsAbs(s.Cwd) {
		return fmt.Sprintf("not probed (cwd: %s resolves per step)", s.Cwd)
	}

	return ""
}

// StaticStatus answers what this machine can say about the server right now without a single request: whether a stdio command is on PATH, and whether a bearer variable is set. The oauth answer is deliberately MCPUnknown rather than a guess — the credential is a token file internal/mcp owns, this package may not import it, and a config package reading a path out of the user's home directory to answer a question about a YAML file would be the wrong thing in the wrong place. Whoever holds the token fills that in; see web.Authorizer.
func (s MCPServer) StaticStatus() MCPStatus {
	if skip := s.NotProbableHere(); skip != "" {
		return MCPStatus{Readiness: MCPUnknown, Detail: skip}
	}

	if s.IsStdio() {
		_, err := exec.LookPath(s.Command)
		if err != nil {
			return MCPStatus{Readiness: MCPMissing, Detail: fmt.Sprintf("command %q not found on PATH", s.Command)}
		}

		return MCPStatus{Readiness: MCPReady, Detail: "on PATH"}
	}

	switch s.Auth.Type {
	case "oauth":
		return MCPStatus{Readiness: MCPUnknown, Detail: "needs a login"}
	case "bearer":
		if detail := unsetEnvDetail(s.Auth.APIKeyEnv); detail != "" {
			return MCPStatus{Readiness: MCPMissing, Detail: detail}
		}

		return MCPStatus{Readiness: MCPReady, Detail: "$" + s.Auth.APIKeyEnv + " is set"}
	default:
		return MCPStatus{Readiness: MCPReady, Detail: "no credential needed"}
	}
}

// unsetEnvDetail is the ONE wording for a missing bearer credential, read by the status cell and by the preflight problem so the page and the refusal cannot disagree. Mirrors mcp.lookupBearerToken: a variable set to the empty string is as unusable as one never set, and a bearer server with no api_key_env cannot authenticate at all.
func unsetEnvDetail(name string) string {
	if name == "" {
		return "auth.type: bearer requires api_key_env"
	}

	if value, ok := os.LookupEnv(name); !ok || value == "" {
		return "$" + name + " is not set (auth.api_key_env)"
	}

	return ""
}

// MCPStatusReason renders a probe failure as a status cell, dropping the copies of the server's name the error carries — the row already names it — because the actionable part of these messages ("run `steps mcp login` …", "$TOKEN is not set") is at the END.
func MCPStatusReason(name string, err error) string {
	reason, _, _ := strings.Cut(err.Error(), "\n")

	for _, noise := range []string{
		"mcp: ",
		fmt.Sprintf("connect to %q: ", name),
		fmt.Sprintf("mcp server %q: ", name),
		fmt.Sprintf("mcp server %q ", name),
	} {
		reason = strings.TrimPrefix(reason, noise)
	}

	return reason
}

// MCPUsers is the "used by" cell: every agent, task fix and resource type that depends on this server, or a phrase saying nothing does.
func (c *Config) MCPUsers(name string) string {
	users := c.MCPServerUsers(name)
	if len(users) == 0 {
		return "(unused)"
	}

	return strings.Join(users, ", ")
}
