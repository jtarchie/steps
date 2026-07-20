package agent

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// subAgentRequestParam is the single argument a sub-agent tool takes: the
// natural-language instruction the parent model authors for the child.
const subAgentRequestParam = "request"

// buildSubAgentTool resolves the child agent named by spec.Agent and returns
// the declaration the parent model sees plus a toolImpl that, on each call,
// runs the child's own fresh tool-calling conversation and returns its final
// text as the tool result.
//
// The child runs in the CALLER's working directory (env.dir at call time) but
// under the CHILD's own resolved image, model, persona, dials, max_turns, and
// tool grant — a sub-agent is a different worker, unlike a fix agent which
// must reproduce the failing task's exact environment. The child conversation
// is not recorded (no merkle node, no job_run): the enclosing agent step
// records the aggregate outcome, and the parent's own call of this tool is
// what shows in its trajectory.
//
// A child failure (transport error, max_turns exhausted, a child required
// tool never succeeding) is returned to the PARENT as {"error": ...} data —
// exactly like any other tool failure, so the parent model can react on its
// next turn rather than the parent attempt being aborted.
//
// The child's LLM client and (recursively) its own tool tree are built eagerly
// here, so a missing credential or a bad grant for a granted sub-agent fails
// the parent step's preparation rather than surfacing only on first call.
// Recursion terminates because LoadConfig's validateAgentGraph rejects cycles
// and caps nesting depth.
func buildSubAgentTool(cfg *config.Config, spec config.ToolSpec) (*genai.FunctionDeclaration, toolImpl, error) {
	if cfg == nil {
		return nil, nil, errors.New("sub-agent tool requires config to resolve the child agent")
	}

	ri, err := cfg.ResolveAgentInvocation(config.Step{Agent: spec.Agent})
	if err != nil {
		return nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	childDecls, childRegistry, err := buildAgentTools(cfg, ri.ToolSpecs, ri.Image)
	if err != nil {
		return nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		return nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	child := preparedSubAgent{
		ri:       ri,
		llm:      newAgentLLM(ri.BaseURL, ri.ModelName, apiKey),
		decls:    childDecls,
		registry: childRegistry,
		required: requiredToolNames(ri.ToolSpecs),
		maxCalls: maxCallsByName(ri.ToolSpecs),
	}

	decl := &genai.FunctionDeclaration{
		Name:        spec.Agent,
		Description: spec.Description,
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				subAgentRequestParam: {Type: genai.TypeString, Description: "The natural-language instruction or question to send to the sub-agent."},
			},
			Required: []string{subAgentRequestParam},
		},
	}

	return decl, child.run, nil
}

// preparedSubAgent holds a child agent's resolved, reusable machinery — built
// once per parent-step preparation (buildSubAgentTool), run once per parent
// tool call (run).
type preparedSubAgent struct {
	ri       config.ResolvedInvocation
	llm      model.LLM
	decls    *genai.Tool
	registry map[string]toolImpl
	required map[string]bool
	maxCalls map[string]int
}

// run executes one child conversation for a parent tool call. env.dir is the
// parent step's working directory; the child's shell tools run there through a
// runner built from the child's own image. It never returns a Go error — a
// child failure comes back to the parent as {"error": ...} data, the same
// contract every toolImpl honours.
func (c preparedSubAgent) run(ctx context.Context, args map[string]any, env toolEnv) map[string]any {
	request := stringArg(args, subAgentRequestParam)
	if request == "" {
		return map[string]any{"error": fmt.Sprintf("%s: missing required argument %q", c.ri.AgentName, subAgentRequestParam)}
	}

	runner, err := shell.NewRunner(c.ri.Image, env.dir)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	conv := agentConversation{
		system: buildSystemMessage(c.ri.Persona, env.dir),
		prompt: request,
		env:    toolEnv{dir: env.dir, runner: runner},
		tools:  agentTools{decls: c.decls, registry: c.registry, required: c.required, maxCalls: c.maxCalls},
		params: agentGenParams{
			temperature: c.ri.Temperature,
			topP:        c.ri.TopP,
			maxTokens:   c.ri.MaxTokens,
			reasoning:   c.ri.ReasoningEffort,
		},
		maxTurns:             c.ri.MaxTurns,
		toolChoiceStringOnly: c.ri.StringOnlyToolChoice,
	}

	res, runErr := runAgentConversation(ctx, c.llm, conv)
	if runErr != nil {
		return map[string]any{"error": runErr.Error()}
	}

	return map[string]any{"result": res.text}
}
