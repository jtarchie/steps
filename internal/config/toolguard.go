package config

// Per-tool guards — required:, max_calls:, args:, max_output_bytes:,
// timeout: — and which tool forms each is valid on.

import (
	"fmt"
	"regexp"
)

// validateToolCallGuards rejects max_calls:/args: set on anything but a
// custom tool: both fields change what a custom tool's run: command executes
// with, and neither has meaning on a builtin (whose implementation is fixed
// Go code) or a sub-agent tool (whose only argument is the model-authored
// request string — pinning or budgeting that is not the shape this feature
// targets; revisit if a real need shows up).
func (c *Config) validateToolCallGuards() error {
	err := c.validateAgentToolCallGuards()
	if err != nil {
		return err
	}

	err = c.validateTaskFixToolCallGuards()
	if err != nil {
		return err
	}

	return c.validateStepToolCallGuards()
}

// toolPosition says where a tools: list was written. It exists for one rule:
// allow: binds only in the grant position, because a step's or fix's list
// SELECTS a granted tool and resolveEffectiveTools resolves that selection by
// substituting the agent's own spec — anything the selection carried is
// dropped on the way through. A fence that silently does not bind is worse
// than one the loader refuses.
type toolPosition int

const (
	// grantPosition is an agents: entry's own tools:, the only place a
	// capability (and therefore its fence) is actually conferred.
	grantPosition toolPosition = iota
	// selectionPosition is a step's or fix's tools:, which may only narrow
	// the set of granted tools, never re-describe one.
	selectionPosition
)

func (c *Config) validateAgentToolCallGuards() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		err := checkToolCallGuardSpecs(fmt.Sprintf("agent %q", agent.Name), grantPosition, agent.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateTaskFixToolCallGuards() error {
	for i := range c.Tasks {
		fix := c.Tasks[i].Fix
		if fix == nil {
			continue
		}

		err := checkToolCallGuardSpecs(fmt.Sprintf("task %q fix", c.Tasks[i].Name), selectionPosition, fix.Tools)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Config) validateStepToolCallGuards() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			err := checkToolCallGuardSpecs(label, selectionPosition, step.Tools)
			if err != nil {
				return err
			}

			if step.Fix == nil {
				return nil
			}

			return checkToolCallGuardSpecs(label+" fix", selectionPosition, step.Fix.Tools)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkToolCallGuardSpecs runs validateToolCallGuardShape over every spec in
// specs, stopping at the first error.
func checkToolCallGuardSpecs(context string, pos toolPosition, specs []ToolSpec) error {
	for _, spec := range specs {
		err := validateToolCallGuardShape(context, spec)
		if err != nil {
			return err
		}

		err = validateWebFetchAllowShape(context, pos, spec)
		if err != nil {
			return err
		}

		err = validateToolTimeoutShape(context, pos, spec)
		if err != nil {
			return err
		}
	}

	return nil
}

// validateToolCallGuardShape rejects max_calls:/args: on a builtin or
// sub-agent tool, a negative max_calls: on any tool, and (since a mapping-
// form builtin — {builtin: name, ...} — is otherwise indistinguishable from a
// custom tool once other fields are present) a builtin mixed with name:/run:.
func validateToolCallGuardShape(context string, spec ToolSpec) error {
	if spec.Builtin != "" && (spec.Name != "" || spec.Run != "") {
		return fmt.Errorf("%s: builtin tool %q must not also set name/run", context, spec.Builtin)
	}

	if spec.MaxCalls < 0 {
		return fmt.Errorf("%s: tool %q: max_calls must be >= 0", context, ToolSpecName(spec))
	}

	err := validateMaxOutputBytesShape(context, spec)
	if err != nil {
		return err
	}

	guarded := spec.MaxCalls != 0 || spec.Args != nil
	if !guarded {
		return nil
	}

	if spec.Builtin != "" {
		return fmt.Errorf("%s: builtin tool %q: max_calls/args are only valid on custom tools", context, spec.Builtin)
	}

	if spec.Agent != "" {
		return fmt.Errorf("%s: sub-agent tool %q: max_calls/args are only valid on custom tools", context, spec.Agent)
	}

	return nil
}

// WebFetchBuiltinName is the one builtin whose grant may carry an allow:
// list. Named as a constant because config and internal/agent must agree on
// the spelling: config validates the shape here, agent binds the list to the
// tool's implementation.
const WebFetchBuiltinName = "web_fetch"

// allowedHostPattern is what an allow: entry must look like: dot-separated
// labels of letters, digits and hyphens. Bare hostnames and IPv4 literals
// pass; a scheme, a path, a port, a comma or a wildcard does not.
//
// Deliberately no IPv6 literal support — url.Hostname() unwraps the brackets,
// so an entry would have to be written bare (`::1`), and no pipeline has
// wanted one. Add it as a case here, with a test, if that changes.
//
//nolint:gochecknoglobals // compiled once, read-only
var allowedHostPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)

// validateWebFetchAllowShape enforces the three things an allow: list must be:
// on the web_fetch builtin (no other tool form has a host to narrow — a custom
// tool's reach is whatever its run: command does, an MCP tool's is its
// server's), in the GRANT position (see toolPosition), and made of bare
// hostnames.
//
// The entry-shape rule is not pedantry. The two backends read the list
// differently — steps compares each entry against url.Hostname(), the claude
// CLI compiles it into WebFetch(domain:…) permission rules — so a
// pattern-shaped entry does not merely fail to match, it can mean OPPOSITE
// things on the two paths. "*" is the worst case: inert here (it equals no
// hostname and suffixes none, so it denies everything), a documented
// match-all wildcard there. Refusing the shape is what keeps one written
// fence from being two different fences.
func validateWebFetchAllowShape(context string, pos toolPosition, spec ToolSpec) error {
	if len(spec.Allow) == 0 {
		return nil
	}

	if spec.Builtin != WebFetchBuiltinName {
		return fmt.Errorf("%s: tool %q: allow is only valid on the %s builtin", context, ToolSpecName(spec), WebFetchBuiltinName)
	}

	if pos != grantPosition {
		return fmt.Errorf(
			"%s: tool %q: allow: binds only where the tool is granted — move it to the agents: entry's tools:, and select it here by bare name",
			context, WebFetchBuiltinName,
		)
	}

	for _, entry := range spec.Allow {
		if !allowedHostPattern.MatchString(entry) {
			return fmt.Errorf(
				"%s: tool %q: allow entry %q must be a bare hostname (no scheme, path, port or wildcard) — a host already covers its subdomains",
				context, WebFetchBuiltinName, entry,
			)
		}
	}

	return nil
}

// validateMaxOutputBytesShape enforces where max_output_bytes: may appear.
// It tunes a tool's inline output budget, which only makes sense for a
// tool whose output volume is not already a designed property: a built-in
// carries its own contract (read_file pages, list_dir counts, search_files
// is bounded by arithmetic), and a sub-agent's result is a considered
// answer rather than a data dump. See ToolSpec.MaxOutputBytes.
func validateMaxOutputBytesShape(context string, spec ToolSpec) error {
	if spec.MaxOutputBytes == 0 {
		return nil
	}

	if spec.MaxOutputBytes < 0 {
		return fmt.Errorf("%s: tool %q: max_output_bytes must be >= 0", context, ToolSpecName(spec))
	}

	if spec.Builtin != "" {
		return fmt.Errorf(
			"%s: builtin tool %q: max_output_bytes is only valid on custom tools and mcp grants; built-ins carry their own output contract",
			context, spec.Builtin,
		)
	}

	if spec.Agent != "" {
		return fmt.Errorf(
			"%s: sub-agent tool %q: max_output_bytes is only valid on custom tools and mcp grants",
			context, spec.Agent,
		)
	}

	return nil
}

// validateToolTimeoutShape enforces the two things a per-tool timeout: must
// be: a positive duration, and written where the tool is GRANTED.
//
// The position rule is the same one allow: has, for the same mechanical
// reason: resolveEffectiveTools resolves a step's bare-name selection by
// substituting the agent's own spec, so a deadline written on the selection
// is dropped on the way through. An INLINE custom tool is the exception —
// a step defines that one rather than selecting it, so its spec is what
// runs, deadline included.
func validateToolTimeoutShape(context string, pos toolPosition, spec ToolSpec) error {
	if spec.Timeout == "" {
		return nil
	}

	d, err := ParseTimeout(spec.Timeout)
	if err != nil {
		return fmt.Errorf("%s: tool %q: %w", context, ToolSpecName(spec), err)
	}

	if d == 0 {
		return fmt.Errorf(
			"%s: tool %q: timeout must be a positive duration (omit it entirely for no per-call deadline)",
			context, ToolSpecName(spec),
		)
	}

	inlineCustom := spec.Name != "" && spec.Run != ""
	if pos != grantPosition && !inlineCustom {
		return fmt.Errorf(
			"%s: tool %q: timeout: binds only where the tool is granted — move it to the agents: entry's tools:, and select it here by bare name",
			context, ToolSpecName(spec),
		)
	}

	return nil
}
