package agent

import (
	"context"
	"fmt"
	"strings"

	"google.golang.org/genai"
)

// verdictToolName is the fixed name of the tool a verdict agent must call to
// emit its routable outcome. A custom tool of the same name on a verdict agent
// is rejected at prepareAgentStep (it would collide with this synthesized one).
const verdictToolName = "verdict"

// buildVerdictTool synthesizes the required `verdict` tool for an agent step
// that declares verdicts:. The model must call it with a `choice` drawn from
// the declared verdicts — the schema enum constrains the model to the
// vocabulary — plus an optional free-text `note` captured for the audit log.
//
// It runs no command: it is a pure signal, so a valid choice always
// "succeeds" (returns exit_code 0, which requiredCallSucceeded recognizes,
// marking the required tool satisfied — no new enforcement code). The
// validated choice is echoed back as "verdict" so runAgentConversation can
// capture it. An out-of-vocabulary choice (shouldn't happen given the enum,
// but defensively) comes back as {"error": ...} data so the model can re-call
// — the same failure-as-data contract as every other tool.
func buildVerdictTool(verdicts []string, noteRequired bool) (*genai.FunctionDeclaration, toolImpl) {
	decl := &genai.FunctionDeclaration{
		Name:        verdictToolName,
		Description: "Emit your decision. Call this exactly once with your final verdict — it is how the pipeline decides which step runs next.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"choice": {Type: genai.TypeString, Enum: append([]string{}, verdicts...), Description: "Your verdict — must be one of the allowed values."},
				"note":   {Type: genai.TypeString, Description: noteDescription(noteRequired)},
			},
			Required: requiredVerdictArgs(noteRequired),
		},
	}

	allowed := make(map[string]bool, len(verdicts))
	for _, verdict := range verdicts {
		allowed[verdict] = true
	}

	impl := func(_ context.Context, args map[string]any, _ toolEnv) map[string]any {
		choice := stringArg(args, "choice")
		if !allowed[choice] {
			return map[string]any{"error": fmt.Sprintf("verdict: choice %q is not one of: %s", choice, strings.Join(verdicts, ", "))}
		}

		result := map[string]any{"exit_code": 0, "verdict": choice}

		note := stringArg(args, "note")
		if note != "" {
			result["note"] = note
		}

		return result
	}

	return decl, impl
}

// injectVerdictTool appends the synthesized required verdict tool to an agent
// step's already-built tool set when the step declares verdicts:, and returns
// the tool's name (or "" when verdict mode is off). A pre-existing tool of the
// same name is a conflict — rejected here rather than silently shadowed.
func injectVerdictTool(verdicts []string, noteRequired bool, decls *genai.Tool, registry map[string]toolImpl, required map[string]bool) (string, error) {
	if len(verdicts) == 0 {
		return "", nil
	}

	if _, exists := registry[verdictToolName]; exists {
		return "", fmt.Errorf("declares verdicts: but already defines a tool named %q", verdictToolName)
	}

	decl, impl := buildVerdictTool(verdicts, noteRequired)
	decls.FunctionDeclarations = append(decls.FunctionDeclarations, decl)
	registry[verdictToolName] = impl
	required[verdictToolName] = true

	return verdictToolName, nil
}

// noteDescription tells the model whether the note is optional, which is
// decided by whether any later step declared context: { from: { this: note } }.
func noteDescription(required bool) string {
	if required {
		return "Required: a short reason. A later step of this pipeline reads it."
	}

	return "Optional: a short reason for the audit log."
}

// requiredVerdictArgs is the verdict tool's required-argument list. The note
// joins it when a downstream reader demanded one — the demand IS the
// obligation, and it is enforced in the tool schema so the model cannot
// satisfy the call without it.
func requiredVerdictArgs(noteRequired bool) []string {
	if noteRequired {
		return []string{"choice", "note"}
	}

	return []string{"choice"}
}
