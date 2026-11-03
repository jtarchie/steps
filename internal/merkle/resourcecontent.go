package merkle

import (
	"github.com/jtarchie/steps/internal/config"
)

// withResourceTypeEnv folds what a resource type's stage command runs as and
// in — image, env names, user, network, isolation, and the backend's own
// stage identity — into content. Shared by the get node, the put node and the
// cross-build cache key so the three cannot drift apart.
//
// The env variable NAMES, never their values: a value is a secret, and this
// map is persisted to state.db. Changing which variables a command can see
// changes what it executes against, so the names are identity; changing a
// value is the operator's environment moving under a pipeline, which this
// package has never claimed to hash (same reasoning as a model's weights).
//
// Unioned with extraEnv (the resource INSTANCE's own env:), not just
// resourceType.Env alone — widening or narrowing one resource's own
// allow-list changes what env() can see for that resource exactly the same
// way editing the type's own env: does, so leaving it out of the hash would
// be the wrong-cache-HIT failure withIsolation's doc comment describes: a get
// cached before a resource's env: named a new variable would be skipped as
// unchanged after, having never run with it visible.
func withResourceTypeEnv(cfg *config.Config, resourceType config.ResourceType, extraEnv []string, stage string, content map[string]any) error {
	if resourceType.Image != "" {
		content["image"] = resourceType.Image
	}

	if env := config.UnionEnv(resourceType.Env, extraEnv); len(env) > 0 {
		content["env"] = sortedEnv(env)
	}

	if resourceType.User != "" {
		content["user"] = resourceType.User
	}

	if resourceType.Network != "" {
		content["network"] = resourceType.Network
	}

	withSandbox(resourceType.Sandbox, content)

	withIsolation(resourceType.Privileged, resourceType.Limits, content)

	withExprResourceStage(resourceType, stage, content)
	withGitHubKind(resourceType, content)

	return withMCPResourceStage(cfg, resourceType, stage, content)
}

// withMCPResourceStage folds an mcp-backed resource type's server identity,
// the named lifecycle stage's tool name AND its args: mapping into content,
// mirroring in_template/out_template's role for the shell backend — but only
// when that stage's *MCPToolCall is actually set (In and Out are both
// optional; an unset In means get falls back to writing version.json, with
// nothing template-shaped to hash, exactly like check: is never hashed for the
// shell backend either). A resource type with no mcp: block is unaffected
// (byte-identical to before this field existed), same as every other
// value-gated field in this file.
//
// args: belongs here for the same reason in_template does: it is what the
// call actually ASKS FOR. Hashing the tool name alone would let a fixed
// mapping — `{{ .version.channel }}` corrected to `{{ .version.thread }}` —
// hash identically to the broken one it replaced, so the get is skipped as
// unchanged and the previously fetched (wrong) artifact is reused. Also
// value-gated, so a stage that names no args: keeps every hash it already has.
func withMCPResourceStage(cfg *config.Config, resourceType config.ResourceType, stage string, content map[string]any) error {
	if resourceType.Config.MCP == nil {
		return nil
	}

	var call *config.MCPToolCall

	switch stage {
	case "in":
		call = resourceType.Config.MCP.In
	case "out":
		call = resourceType.Config.MCP.Out
	}

	if call == nil {
		return nil
	}

	server, err := mcpServerContent(cfg, resourceType.Config.MCP.Server)
	if err != nil {
		return err
	}

	content["mcp_"+stage+"_tool"] = call.Tool
	content["mcp_server"] = server

	if len(call.Args) > 0 {
		// Normalized, not stored raw: yaml.v3 decodes a mapping whose keys
		// are not all strings into map[any]any (`args: {filters: {2024:
		// "yes"}}`, an unquoted `on:`), which encoding/json cannot marshal —
		// so HashNode would fail the whole plan with "json: unsupported type"
		// on a pipeline that loads, validates, and RUNS fine, since
		// resource.renderArgValue handles exactly that shape. The hash has to
		// accept every args: the runtime accepts.
		content["mcp_"+stage+"_args"] = normalizeMapKeys(call.Args)
	}

	return nil
}

// withExprResourceStage folds an expr-backed type's program text into the
// content map, the same way in_template/out_template does for shell.
//
// Gated on the BACKEND rather than on the text being non-empty: switching a
// type from shell to expr with neither declaring an in: must still bust the
// hash, because the two fetch differently even when both are blank. Types
// that are not expr-backed fold nothing, so every hash already in a state.db
// stays byte-identical.
func withExprResourceStage(resourceType config.ResourceType, stage string, content map[string]any) {
	if resourceType.Config.Expr == nil {
		return
	}

	switch stage {
	case "in":
		content["expr_in"] = resourceType.Config.Expr.In
	case "out":
		content["expr_out"] = resourceType.Config.Expr.Out
	}
}

// withGitHubKind folds which github-* type a resource is into its node. The
// four share one backend and two share a source: shape, so without it a
// github-prs get and a github-comments get of the same repo would differ
// only by version, and a version that happened to match would serve one
// type's tree as the other's. Value-gated: every other type hashes as before.
func withGitHubKind(resourceType config.ResourceType, content map[string]any) {
	if resourceType.Config.GitHub != "" {
		content["github"] = resourceType.Config.GitHub
	}
}
