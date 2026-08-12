package agent

import (
	"context"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// synthesizedTools is what injectSynthesizedTools produced beyond the tool
// set it mutated in place: the verdict tool's name (see injectVerdictTool),
// "" when the step declares no verdicts:.
type synthesizedTools struct {
	verdictTool string
}

// injectSynthesizedTools adds the required verdict tool onto an
// already-built tool set, when step declares verdicts:.
func injectSynthesizedTools(
	_ context.Context, _ *config.Config, step config.Step,
	decls *genai.Tool, registry map[string]toolImpl, required map[string]bool,
) (synthesizedTools, error) {
	verdictTool, err := injectVerdictTool(step.VerdictNames(), step.NoteRequired, decls, registry, required)
	if err != nil {
		return synthesizedTools{}, err
	}

	return synthesizedTools{verdictTool: verdictTool}, nil
}
