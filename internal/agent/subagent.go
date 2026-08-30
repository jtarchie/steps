package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

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
// what shows in its trajectory. Its response is still echoed to the
// terminal, labeled "agent: <name> (sub-agent)", the same as any other
// agent conversation (see printAgentResponse) — otherwise a human watching
// the run only ever sees the parent's own summary of what the child said.
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
func buildSubAgentTool(ctx context.Context, cfg *config.Config, spec config.ToolSpec) (*genai.FunctionDeclaration, toolImpl, io.Closer, error) {
	if cfg == nil {
		return nil, nil, nil, errors.New("sub-agent tool requires config to resolve the child agent")
	}

	// Resolved through the failover seam, not bare: preflight deliberately
	// probes sub-agents too (see withSubAgents), and on a dead primary it
	// picks a fallback, pins it, and reports the job healthy. Resolving the
	// child without consulting that selection meant the run then went to the
	// primary preflight had just proved dead — preflight making a promise the
	// runtime did not keep. A sub-agent still gets no MID-RUN cascade of its
	// own; the delegation is one conversation on the source preflight chose.
	_, ri, _, _, err := resolveWithFailover(ctx, cfg, config.Step{Agent: spec.Agent})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	childTools, childClosers, err := buildAgentTools(ctx, cfg, ri.ToolSpecs, ri.Image)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	apiKey, err := lookupAPIKey(ri.APIKeyEnv, ri.RequiresKey)
	if err != nil {
		closeAll(childClosers)

		return nil, nil, nil, fmt.Errorf("sub-agent %q: %w", spec.Agent, err)
	}

	child := preparedSubAgent{
		ri:       ri,
		llm:      newAgentLLM(ri, apiKey),
		tools:    childTools,
		fraction: cfg.DelegateBudgetFraction(spec.Agent),
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

	var closer io.Closer
	if len(childClosers) > 0 {
		closer = multiCloser(childClosers)
	}

	return decl, child.run, closer, nil
}

// preparedSubAgent holds a child agent's resolved, reusable machinery — built
// once per parent-step preparation (buildSubAgentTool), run once per parent
// tool call (run).
type preparedSubAgent struct {
	ri    config.ResolvedInvocation
	llm   model.LLM
	tools agentTools
	// fraction is how much of THIS agent's remaining allowance one of ITS
	// own sub-agent calls may take (config.DelegateBudgetFraction), carried
	// so a grandchild is sized against its immediate parent's setting.
	fraction float64
}

// allowanceFrom sizes this delegation against what the parent has left, and
// refuses one the parent can no longer fund.
//
// Refusing rather than proceeding uncapped is the whole point: an agent that
// has spent its allowance must not be able to buy more of it by delegating.
// The error names both numbers, since "why did my helper not run" is
// otherwise answerable only by reading the token log.
//
// No parent accumulator (a sub-agent invoked outside a conversation, which
// only tests do) leaves the child on its own declared budget, as before. So
// does a parent with NO ceiling: there is no allowance to take a fraction of,
// and 0 means unbounded rather than empty — reading it as empty refused every
// delegation on every pipeline that declares no budgets, which is most of
// them, while telling the model its parent's budget was spent.
func (c preparedSubAgent) allowanceFrom(parent *stepUsage) (int, error) {
	if parent == nil || !parent.hasCeiling() {
		return c.ri.BudgetTokens, nil
	}

	allowance := parent.delegatedBudget(c.ri.BudgetTokens)
	if allowance <= 0 {
		return 0, fmt.Errorf("%s: the delegating agent has no token allowance left to fund this call (its budget: is spent, including what earlier delegations took) — finish without this helper",
			c.ri.AgentName)
	}

	return allowance, nil
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

	// Sized before any work starts: a delegation the parent cannot fund is
	// refused outright rather than run against a ceiling it has already
	// crossed. The model sees the refusal as tool-result data and can finish
	// without the helper, which is the same contract every other child
	// failure honours.
	allowance, err := c.allowanceFrom(env.usage)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	runner, err := shell.NewRunner(shell.RunnerSpec{Image: c.ri.Image, Cwd: env.dir, Env: c.ri.Env, User: c.ri.User, Network: c.ri.Network,
		Privileged: c.ri.Privileged, CPUShares: c.ri.Limits.CPUShares(), MemoryBytes: c.ri.Limits.MemoryBytes()})
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	runner = runner.WithLabel(c.ri.AgentName)
	defer shell.CloseRunner(runner, c.ri.AgentName)

	// context_paths is step-level only (not inherited by sub-agents), so
	// c.ri.ContextPaths is always empty here — loadContextBlocks still
	// resolves nil/empty safely. A bad path arrives as ordinary tool-result
	// data, the same contract every child failure honours.
	contextBlocks, err := loadContextBlocks(env.dir, c.ri.ContextPaths, c.ri.MaxContextBytes)
	if err != nil {
		return map[string]any{"error": fmt.Sprintf("%s: %s", c.ri.AgentName, err)}
	}

	conv := agentConversation{
		system:        buildSystemMessage(c.ri.Persona, env.dir),
		messages:      []string{request},
		contextBlocks: contextBlocks,
		// The parent's ask context travels down, renamed: a sub-agent granted
		// ask_user is asking on behalf of a real, recorded run, and without
		// this it was told there was nobody to ask on a run that manifestly
		// had somebody. The NAME is the child's, so a parked question says
		// which agent wants to know rather than which one delegated.
		env:   toolEnv{dir: env.dir, runner: runner, spillDir: env.spillDir, ask: env.ask.forAgent(c.ri.AgentName)},
		tools: c.tools,
		params: agentGenParams{
			temperature: c.ri.Temperature,
			topP:        c.ri.TopP,
			maxTokens:   c.ri.MaxTokens,
			reasoning:   c.ri.ReasoningEffort,
		},
		maxTurns:             c.ri.MaxTurns,
		toolChoiceStringOnly: c.ri.StringOnlyToolChoice,
		// A child recorder off the parent's: the delegation's turns publish
		// live, one level deeper, instead of surfacing only when the child
		// finishes and the parent summarizes it.
		recorder: env.transcript.childRecorder(),
		// A sub-agent DRAWS ON its parent's allowance rather than adding to
		// it: its ceiling is a share of what the parent has left, capped by
		// whatever it declared for itself, and its spend is charged back up
		// the chain when it finishes (stepUsage.finish). That is what makes
		// an agent's budget: a bound on the whole subtree instead of on one
		// conversation in it — without it a capped agent could delegate its
		// way past its own ceiling without ever exceeding it.
		//
		// It is still reported under its own name and still rolls into the
		// job total exactly once, as before.
		usage: &stepUsage{
			name: c.ri.AgentName, budget: allowance, parent: env.usage,
			delegateFraction: c.fraction,
		},
	}
	// A sub-agent conversation owns its own stepUsage's whole lifetime, the
	// same as RunFix and runPreparedWithFailover — runConversationLoop no
	// longer calls finish() on a caller's behalf (see its own doc comment).
	// Without this, chargeDelegated never fires: the parent's budget is never
	// debited for what this delegation spent, and the child's own spend never
	// reaches the job total.
	defer conv.usage.finish()

	fmt.Printf("agent: %s (sub-agent)\n", c.ri.AgentName)

	// Which run/job/step this delegation belongs to and how deep it nests —
	// read from the PARENT's live context (conv.recorder is the child's own,
	// one level deeper), so a watcher reading slog output can attribute a
	// child conversation to the step that spawned it, not just the events
	// bus (see conv.recorder above / transcriptRecorder.childRecorder).
	live := env.transcript.liveIdentity()
	started := time.Now()

	slog.Info("agent.subagent_start", "run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex,
		"depth", live.depth+1, "agent", c.ri.AgentName)

	// The child gets its own request counter so its provider requests are
	// attributed to it rather than silently folded into the parent's total.
	// Its session is stable per (run, agent) with no attempt component, so
	// repeated calls with the identical prompt keep a warm cache (see
	// composeSessionID).
	res, runErr := runAgentConversation(withRequestCounter(ctx, &requestCounter{}), c.llm, conv)
	printAgentResponse(res)

	slog.Info("agent.subagent_finish", "run", live.runID, "job", live.job, "step", live.stepName, "index", live.stepIndex,
		"depth", live.depth+1, "agent", c.ri.AgentName, "duration", time.Since(started), "error", runErr)

	// Nest the child's transcript into the PARENT's recorder (env.transcript
	// is the caller's), before the error branch: a failed child's trace is
	// the one worth reading afterwards.
	env.transcript.subagent(c.ri.AgentName, request, res.transcript)

	if runErr != nil {
		return map[string]any{"error": runErr.Error()}
	}

	// Cap the child's answer like every other tool result — a chatty child with
	// no max_tokens must not flood the parent's context past maxToolOutputBytes.
	// Spills to a file (like shellToolResult/MCP) rather than dropping the
	// overflow, using the parent's own spill directory.
	return map[string]any{"result": spillOrTruncate(res.text, env.spillDir)}
}
