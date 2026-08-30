package config

// The ask_user builtin: which tool forms its dials are valid on, and what an
// answered_by: responder is allowed to be.

import "fmt"

// AskUserBuiltinName is the builtin that lets an agent say it does not know
// something and get an answer. Named as a constant for the reason
// WebFetchBuiltinName is: config validates the grant's shape, internal/agent
// binds the dials to the implementation, and the two must agree on the
// spelling.
//
// It is deliberately absent from DefaultAgentToolSpecs. Interrupting a person
// is a capability a pipeline hands over explicitly, exactly like the ability
// to write a file.
const AskUserBuiltinName = "ask_user"

// BuiltinIsNeverNativeToCLI reports whether a coding-agent CLI can never run
// this builtin itself, so a grant of it is always bridged back to this
// process.
//
// It exists because two load-time guards were written when every builtin WAS
// native to the CLI, and each reads a non-native one wrong in the opposite
// direction — see checkCLIAgentTools. ask_user is the first such builtin: a
// CLI has no equivalent that could work (an answer would land in the child's
// transcript rather than in the parent, where the questions row, the memo and
// the responder ladder all live), so the bridge is the only implementation
// there is, and the dials bound to it therefore DO bind.
//
// internal/agent's cliRuntimes natives table is the authority on the other
// direction; TestNonNativeBuiltinsAreNotClaimedAsNative keeps the two honest.
func BuiltinIsNeverNativeToCLI(name string) bool {
	return name == AskUserBuiltinName
}

// validateAskUserShape enforces where ask_user's own dials may appear: on the
// ask_user builtin, in the GRANT position, and nowhere else.
//
// The grant-position rule is the one that would otherwise bite silently. A
// step's tools: SELECTS from what the agent granted, and resolveEffectiveTools
// resolves that selection by substituting the agent's own spec — so an
// answered_by: written on a step would read like a routing decision and bind
// nothing at all. That is the same fence allow: already has, for the same
// reason.
func validateAskUserShape(context string, pos toolPosition, spec ToolSpec) error {
	if spec.AnsweredBy == "" && spec.Default == "" && !spec.OptionsRequired {
		return nil
	}

	if spec.Builtin != AskUserBuiltinName {
		return fmt.Errorf("%s: tool %q: answered_by/default/options_required are only valid on the %s builtin",
			context, ToolSpecName(spec), AskUserBuiltinName)
	}

	if pos != grantPosition {
		return fmt.Errorf(
			"%s: tool %q: answered_by/default/options_required bind only where the tool is granted — move them to the agents: entry's tools:, and select it here by bare name",
			context, AskUserBuiltinName,
		)
	}

	return nil
}

// validateAskUserResponders checks every answered_by: names an agent that can
// actually answer.
//
// The no-asking rule is what keeps the ladder acyclic without a graph walk: a
// responder ANSWERS, so it may not itself ask. Without it, two agents naming
// each other would escalate forever, and the cheapest honest fence is the one
// that makes the cycle unrepresentable rather than merely detected.
//
// A cli source is refused for the reason a sub-agent grant is: a responder
// runs a nested conversation inside the asking step's turn loop, and a CLI
// agent replaces that loop wholesale.
func (c *Config) validateAskUserResponders() error {
	for i := range c.Agents {
		for _, spec := range c.Agents[i].Tools {
			err := c.checkAskUserResponder(c.Agents[i].Name, spec)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Config) checkAskUserResponder(asker string, spec ToolSpec) error {
	if spec.AnsweredBy == "" {
		return nil
	}

	responder, err := c.FindAgent(spec.AnsweredBy)
	if err != nil {
		return fmt.Errorf("agent %q: answered_by: %w", asker, err)
	}

	if agentUsesCLI(*responder) {
		return fmt.Errorf("agent %q: answered_by: %q is a cli source (%s...), which runs its own tool loop and cannot answer inside this step's conversation; name a hosted agent",
			asker, responder.Name, CLISourcePrefix)
	}

	for _, granted := range responder.Tools {
		if granted.Builtin == AskUserBuiltinName {
			return fmt.Errorf("agent %q: answered_by: %q itself grants %s; a responder answers, it does not ask",
				asker, responder.Name, AskUserBuiltinName)
		}
	}

	return nil
}
