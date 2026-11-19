package config

// What can be said about an mcp_servers: entry without asking it anything. It lives here, not in internal/mcpstatus with the rest of the front ends' vocabulary, because CheckEnvironment refuses on the same answer the status cell prints.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// MCPReadiness is what is knowable about a server with no request made, and the reason a status cell says what it says.
type MCPReadiness int

const (
	// MCPReady is every static condition met: a stdio command found on PATH, a bearer variable set, or an unauthenticated endpoint, which has nothing to check.
	MCPReady MCPReadiness = iota
	// MCPMissing is a static condition that FAILS, and that waiting will not fix: the command is not on PATH, or the variable is not set.
	MCPMissing
	// MCPUnknown is a server whose readiness this package cannot answer — an oauth server's credential is a login internal/mcp keeps, and a stdio server with a relative cwd: resolves per step.
	MCPUnknown
)

// MCPStatus is one server's static status: whether it is usable, and the phrase both front ends print for it.
type MCPStatus struct {
	Readiness MCPReadiness
	// Detail is the whole cell, written for somebody who has to act on it — `$GITHUB_PAT is not set`, not `false`.
	Detail string
}

// NotProbableHere reports the status for a server that cannot honestly be probed from outside a run, or "" for one that can. A relative cwd: is resolved against the working directory of the agent step whose tools are being built (WithResolvedMCPCwd) — a build workspace that exists only during a run, so spawning it from wherever the operator happens to be would chdir somewhere else entirely and report a server that works perfectly in a run as broken.
func (s MCPServer) NotProbableHere() string {
	if s.IsStdio() && s.Cwd != "" && !filepath.IsAbs(s.Cwd) {
		return fmt.Sprintf("not probed (cwd: %s resolves per step)", s.Cwd)
	}

	return ""
}

func (s MCPServer) stdioStatus() MCPStatus {
	// This machine's PATH says nothing about the image's, and asking the image would mean pulling it.
	if s.Image != "" {
		return MCPStatus{Readiness: MCPUnknown, Detail: "runs in image " + s.Image}
	}

	_, err := exec.LookPath(s.Command)
	if err != nil {
		return MCPStatus{Readiness: MCPMissing, Detail: fmt.Sprintf("command %q not found on PATH", s.Command)}
	}

	return MCPStatus{Readiness: MCPReady, Detail: "on PATH"}
}

// StaticStatus answers what this machine can say about the server right now without a single request: whether a stdio command is on PATH, and whether a bearer variable is set. The oauth answer is deliberately MCPUnknown rather than a guess — the credential is a login internal/mcp keeps, sealed in the state database, and this package may not import it. Whoever holds the token fills that in; see web.Authorizer.
func (s MCPServer) StaticStatus() MCPStatus {
	if skip := s.NotProbableHere(); skip != "" {
		return MCPStatus{Readiness: MCPUnknown, Detail: skip}
	}

	if s.IsStdio() {
		return s.stdioStatus()
	}

	switch s.Auth.Type {
	case MCPAuthOAuth:
		return MCPStatus{Readiness: MCPUnknown, Detail: "needs a login"}
	case MCPAuthBearer:
		// A token the daemon resolves from a secret source is never read from its environment.
		if s.Auth.TokenSecret() != "" {
			return MCPStatus{Readiness: MCPReady, Detail: "$" + s.Auth.APIKeyEnv + " comes from a secret source"}
		}

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
