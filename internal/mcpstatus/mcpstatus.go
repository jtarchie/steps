// Package mcpstatus is how an mcp_servers: entry is DESCRIBED to a reader. Both front ends render it — `steps mcp list` as a table, the web UI's mcp tab as a page — and it is its own leaf because internal/web cannot import internal/cli: two front ends over one concept with no shared vocabulary would drift into two.
package mcpstatus

import (
	"fmt"
	"strings"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/redact"
)

// Transport names which of the two shapes this server is.
func Transport(s config.MCPServer) string {
	if s.IsStdio() {
		return "stdio"
	}

	return "http"
}

// Target renders what the server actually is, for a reader: the endpoint for HTTP, the argv (plus any pinned working directory) for stdio. URL credentials are masked, because argv pasted from vendor docs carries tokens as userinfo and this string is drawn on a page every viewer of the daemon can read; nothing spawns from it.
func Target(s config.MCPServer) string {
	if !s.IsStdio() {
		return redact.Command(s.Endpoint)
	}

	target := strings.Join(append([]string{s.Command}, s.Args...), " ")
	if s.Cwd != "" {
		target += fmt.Sprintf(" (cwd: %s)", s.Cwd)
	}

	if s.Image != "" {
		target += fmt.Sprintf(" (image: %s)", s.Image)
	}

	return redact.Command(target)
}

// AuthLabel names the auth type and, for bearer, the environment variable the credential is read from — the thing to go check when it is the credential that is missing. Never the value.
func AuthLabel(s config.MCPServer) string {
	if s.Auth.None() {
		return config.MCPAuthNone
	}

	if s.Auth.APIKeyEnv != "" {
		return s.Auth.Type + " $" + s.Auth.APIKeyEnv
	}

	return s.Auth.Type
}

// Reason renders a probe failure as a status cell, dropping the copies of the server's name the error carries — the row already names it — because the actionable part of these messages ("run `steps mcp login` …", "$TOKEN is not set") is at the END.
func Reason(name string, err error) string {
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

// Users is the "used by" cell: every agent, task fix and resource type that depends on this server, or a phrase saying nothing does.
func Users(c *config.Config, name string) string {
	users := c.MCPServerUsers(name)
	if len(users) == 0 {
		return "(unused)"
	}

	return strings.Join(users, ", ")
}
