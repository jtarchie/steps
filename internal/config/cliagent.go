package config

// CLI-backed agent sources: a source.model spelled "@claude/sonnet" runs a
// coding-agent CLI as a subprocess instead of reaching an OpenAI-compatible
// endpoint over HTTP.
//
// The distinction is delegation, not transport. An HTTP source hands
// internal/agent a model to drive a tool-calling loop against; a CLI source
// hands it a program that owns its own loop. What steps keeps either way is
// everything AROUND the conversation — workspace, merkle caching, timeout,
// store recording, verdict routing — which is why this is a new kind of
// source rather than a new kind of step.
//
// This file owns only what a LOAD needs to know: which "@name" prefixes exist,
// how a model string splits, and which agent-level settings have no meaning
// once the CLI is driving. The invocation details (which native tools a grant
// maps to, how the process is spawned) live in internal/agent, mirroring how
// agentProviders keeps its endpoint table here and its request behavior there.

import (
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
)

// CLISourcePrefix marks a model string as naming a CLI rather than a hosted
// provider. Chosen because no provider prefix can contain it, so the two
// namespaces cannot collide as either table grows.
const CLISourcePrefix = "@"

// CLISettingsProject is the only accepted value of Agent.Settings: the CLI
// subprocess loads the repo's checked-in project scope (.claude/ settings,
// CLAUDE.md, hooks). Absent, it loads no settings at all.
const CLISettingsProject = "project"

// cliProvider is a coding-agent CLI steps knows how to drive.
type cliProvider struct {
	// binary is the executable name looked up on PATH.
	binary string
}

// cliProviders is the set of recognized "@name" sources. Keyed without the
// prefix, so "@claude/sonnet" resolves through key "claude".
//
// Adding an entry here is deliberately not enough to make one work:
// internal/agent keeps the matching argument/tool mapping and a test asserts
// the two tables agree, so a half-added CLI fails the build rather than a run.
//
//nolint:gochecknoglobals // static, read-only lookup table
var cliProviders = map[string]cliProvider{
	"claude": {binary: "claude"},
}

// CLIBinary reports the executable backing a CLI source, or "" for an
// unrecognized one. Exported for internal/agent (spawning, preflight) and for
// this package's own PATH check.
func CLIBinary(cli string) string {
	return cliProviders[cli].binary
}

// CLIProviderNames lists every recognized CLI key. Exported so internal/agent
// can assert its own table covers exactly these.
func CLIProviderNames() []string {
	names := make([]string, 0, len(cliProviders))
	for name := range cliProviders {
		names = append(names, name)
	}

	return names
}

// IsCLISource reports whether a source names a CLI rather than a hosted
// provider. It answers the question by SPELLING, not by resolvability, so a
// misspelled "@cluade/sonnet" is still recognized as a CLI source and gets the
// CLI-specific error instead of the generic "no known provider prefix".
func IsCLISource(source AgentSource) bool {
	return strings.HasPrefix(source.Model, CLISourcePrefix)
}

// resolveCLITarget interprets a "@cli/model" source. The model half is passed
// through verbatim (the CLI names its own models — "sonnet", "opus"), which is
// also what makes it the value merkle hashes as `model`.
//
// endpoint: is rejected rather than ignored: there is no HTTP request to point
// anywhere, so accepting it would let a pipeline read as if it were configured
// while the setting did nothing.
func resolveCLITarget(source AgentSource) (agentTarget, error) {
	name, rest, hasSlash := strings.Cut(strings.TrimPrefix(source.Model, CLISourcePrefix), "/")

	if _, known := cliProviders[name]; !known {
		return agentTarget{}, fmt.Errorf("model %q names no known cli; supported: %s",
			source.Model, strings.Join(slices.Sorted(maps.Keys(cliProviders)), ", "))
	}

	if !hasSlash || rest == "" {
		return agentTarget{}, fmt.Errorf("model %q must name a model after the cli, e.g. %s%s/sonnet",
			source.Model, CLISourcePrefix, name)
	}

	if source.Endpoint != "" {
		return agentTarget{}, fmt.Errorf("model %q is a cli source and has no endpoint to configure; remove source.endpoint", source.Model)
	}

	// An api_key_env: on a CLI source is forwarded to the subprocess rather
	// than sent as a header. Leaving it unset is the common case — the CLI
	// authenticates from its own credential store — so a key is only required
	// when the pipeline explicitly names one, matching how an explicit
	// api_key_env: is treated on an endpoint-only HTTP source.
	return agentTarget{
		ModelName:   rest,
		APIKeyEnv:   source.APIKeyEnv,
		RequiresKey: source.APIKeyEnv != "",
		CLI:         name,
	}, nil
}

// validateCLIAgents rejects settings that have no meaning once a CLI owns the
// conversation. Every one of them would otherwise be silently ignored — the
// pipeline would read as if temperature: or a tool guard were in force while
// nothing enforced it, which is the failure this package exists to prevent.
//
// It checks an agent whose PRIMARY or any FALLBACK source is a CLI, since a
// setting that cannot survive failover is not configured either.
func (c *Config) validateCLIAgents() error {
	for i := range c.Agents {
		agent := c.Agents[i]
		if !agentUsesCLI(agent) {
			if agent.Settings != "" {
				return fmt.Errorf("agent %q: settings is only supported with a cli source (%s...); a hosted provider has no CLI configuration to load — remove it",
					agent.Name, CLISourcePrefix)
			}

			continue
		}

		if agent.Settings != "" && agent.Settings != CLISettingsProject {
			return fmt.Errorf("agent %q: settings %q is not supported; the only value is %q (load the repo's checked-in .claude/ scope) — absent loads none",
				agent.Name, agent.Settings, CLISettingsProject)
		}

		err := checkCLIAgentSettings(agent)
		if err != nil {
			return err
		}

		err = c.checkCLIAgentTools(agent)
		if err != nil {
			return err
		}
	}

	err := c.checkHostedAgentBudgets()
	if err != nil {
		return err
	}

	return c.checkCLIAgentReferences()
}

// checkHostedAgentBudgets is the mirror of the budget.tokens rejection above:
// a dollar ceiling has no meaning for an agent this process drives, because
// there is no cost figure on the wire to compare it against. Rejecting it
// keeps budget: honest in both directions rather than letting one spelling
// sit there doing nothing.
func (c *Config) checkHostedAgentBudgets() error {
	for i := range c.Agents {
		agent := c.Agents[i]

		if budgetUSD(agent.Budget) > 0 && !agentUsesCLI(agent) {
			return fmt.Errorf("agent %q: budget.usd is only supported with a cli source (%s...); a hosted provider reports tokens, not dollars — use budget.tokens",
				agent.Name, CLISourcePrefix)
		}
	}

	for _, job := range c.Jobs {
		if budgetUSD(job.Budget) > 0 {
			return fmt.Errorf("job %q: budget.usd is not supported; a job budget is cumulative across mixed step kinds — use budget.tokens", job.Name)
		}
	}

	return nil
}

// agentUsesCLI reports whether any source this agent could run under is a CLI.
func agentUsesCLI(agent Agent) bool {
	if IsCLISource(agent.Source) {
		return true
	}

	for i := range agent.Fallback {
		if IsCLISource(agent.Fallback[i].Source) {
			return true
		}
	}

	return false
}

// checkCLIAgentSettings rejects the agent-level dials and knobs a CLI cannot
// honor. Grouped into one table so the reason each is unsupported is stated
// once, next to the setting.
func checkCLIAgentSettings(agent Agent) error {
	unsupported := []struct {
		set   bool
		field string
		why   string
	}{
		{agent.Temperature != nil, "temperature", "the cli chooses its own sampling"},
		{agent.TopP != nil, "top_p", "the cli chooses its own sampling"},
		{agent.MaxTokens > 0, "max_tokens", "the cli manages its own output limits"},
		{agent.Source.StringToolChoice != nil, "source.string_tool_choice", "there is no tool_choice on the wire to spell"},
		{agent.CompactAfterTokens != nil, "compact_after_tokens", "the cli compacts its own conversation"},
		{agent.ContextWindow > 0, "context_window", "the cli compacts its own conversation, against a window it resolves itself"},
		// Not budget itself: a CLI agent takes budget.usd, which the CLI
		// enforces mid-run. Only the token spelling is unenforceable here,
		// since nothing counts tokens until the subprocess has already exited.
		{budgetTokens(agent.Budget) > 0, "budget.tokens", "nothing counts tokens until the subprocess exits; use budget.usd, which the cli enforces mid-run"},
	}

	for _, u := range unsupported {
		if u.set {
			return fmt.Errorf("agent %q: %s is not supported with a cli source (%s...); %s — remove it or use a hosted provider",
				agent.Name, u.field, CLISourcePrefix, u.why)
		}
	}

	return nil
}

// checkCLIAgentTools rejects tool grants a CLI agent cannot enforce. The tool
// GUARDS are the load-bearing ones: required:/max_calls:/args: are enforced by
// internal/agent's own turn loop, which a CLI agent does not run, so accepting
// them would promise a constraint nothing applies. max_output_bytes survives —
// it is enforced inside the tool implementation, which the bridge reuses.
//
// timeout: splits along that same line, which is why it is only half
// rejected: a custom or MCP tool is BRIDGED (the child calls the very impl
// the deadline is bound to, so it holds), while a NATIVE built-in is run by
// the CLI itself — the bridge never sees the call, and a deadline written
// there would be a fence that silently does not bind.
//
// "Native" is load-bearing and was once assumed rather than asked. ask_user
// is the first builtin no CLI runs itself (BuiltinIsNeverNativeToCLI): the
// child calls the parent's own impl over the bridge, so the deadline DOES
// apply — and refusing it would deny a CLI agent the one dial that decides
// how long a person is waited on.
func (c *Config) checkCLIAgentTools(agent Agent) error {
	for _, spec := range agent.Tools {
		name := ToolSpecName(spec)

		switch {
		case spec.Required:
			return fmt.Errorf("agent %q: tool %q sets required, which is not supported with a cli source (%s...); the cli decides its own tool calls",
				agent.Name, name, CLISourcePrefix)
		case spec.MaxCalls > 0:
			return fmt.Errorf("agent %q: tool %q sets max_calls, which is not supported with a cli source (%s...); the cli owns its own tool loop",
				agent.Name, name, CLISourcePrefix)
		case len(spec.Args) > 0:
			return fmt.Errorf("agent %q: tool %q sets args, which is not supported with a cli source (%s...); pin the value inside the tool's run: instead",
				agent.Name, name, CLISourcePrefix)
		case spec.Timeout != "" && spec.Builtin != "" && !BuiltinIsNeverNativeToCLI(spec.Builtin):
			return fmt.Errorf("agent %q: builtin tool %q sets timeout, which is not supported with a cli source (%s...); the cli runs its built-ins itself — bound a custom or mcp tool instead, or the step",
				agent.Name, name, CLISourcePrefix)
		case spec.Agent != "":
			return fmt.Errorf("agent %q: tool %q grants a sub-agent, which is not supported with a cli source (%s...); delegate with a separate agent step instead",
				agent.Name, name, CLISourcePrefix)
		}
	}

	return nil
}

// checkCLIAgentReferences rejects the two places a CLI agent can be named by
// something that would have to drive its conversation directly: as another
// agent's sub-agent tool, and as a task's fix: agent. Both build their
// conversation on internal/agent's turn loop, which a CLI source replaces
// wholesale.
func (c *Config) checkCLIAgentReferences() error {
	isCLI := func(name string) bool {
		agent, err := c.FindAgent(name)

		return err == nil && agentUsesCLI(*agent)
	}

	err := c.checkNoCLISubAgents(isCLI)
	if err != nil {
		return err
	}

	err = c.checkNoCLIFixAgents(isCLI)
	if err != nil {
		return err
	}

	return c.checkCLIContainerNetwork()
}

// checkNoCLISubAgents rejects a CLI agent named by any sub-agent grant. A
// sub-agent runs a nested conversation inside the parent's turn loop, and a
// CLI agent has none to nest into.
func (c *Config) checkNoCLISubAgents(isCLI func(string) bool) error {
	for i := range c.Agents {
		for _, spec := range c.Agents[i].Tools {
			if spec.Agent != "" && isCLI(spec.Agent) {
				return fmt.Errorf("agent %q: tool %q delegates to agent %q, which has a cli source (%s...); a cli agent cannot run as a sub-agent",
					c.Agents[i].Name, ToolSpecName(spec), spec.Agent, CLISourcePrefix)
			}
		}
	}

	return nil
}

// checkNoCLIFixAgents rejects a CLI agent as a fix: agent, declared on a task
// or inline on a step. RunFix builds its conversation on the same turn loop a
// sub-agent does.
func (c *Config) checkNoCLIFixAgents(isCLI func(string) bool) error {
	for _, task := range c.Tasks {
		if task.Fix != nil && task.Fix.Agent != "" && isCLI(task.Fix.Agent) {
			return fmt.Errorf("task %q fix: names agent %q, which has a cli source (%s...); a cli agent cannot run as a fix agent",
				task.Name, task.Fix.Agent, CLISourcePrefix)
		}
	}

	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			return checkCLIStepReferences(label, step, isCLI)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// checkCLIStepReferences rejects a step's own inline fix: agent being a CLI
// one.
func checkCLIStepReferences(label string, step *Step, isCLI func(string) bool) error {
	if step.Fix != nil && step.Fix.Agent != "" && isCLI(step.Fix.Agent) {
		return fmt.Errorf("%s: fix: names agent %q, which has a cli source (%s...); a cli agent cannot run as a fix agent",
			label, step.Fix.Agent, CLISourcePrefix)
	}

	return nil
}

// checkCLIContainerNetwork rejects `network: none` on a containerized CLI
// agent step.
//
// A CLI agent's non-native tools — the synthesized verdict/context tools
// among them, not just custom run: ones — reach the child over a
// loopback MCP server this process hosts (see internal/agent's clibridge).
// Containerizing the CLI means that connection has to cross out of the
// container, so cutting the container's network does not merely narrow what
// the agent can reach: it removes the channel the step's own verdict comes
// back on. That is a step which cannot possibly succeed, and it is worth
// saying so at load rather than after the run burns its budget.
//
// This is distinct from the same setting on an HTTP agent, where `network:
// none` is a perfectly coherent (and useful) way to sandbox model-written
// commands — nothing there depends on egress.
func (c *Config) checkCLIContainerNetwork() error {
	for i := range c.Agents {
		agent := &c.Agents[i]
		if !agentUsesCLI(*agent) {
			continue
		}

		err := c.visitAgentSteps(agent.Name, func(label string, step *Step) error {
			settings := resolveAgentRuntime(agent, *step)
			if settings.Image != "" && settings.Network == noNetwork {
				return fmt.Errorf("%s: network %q is not supported on a containerized agent %q, which has a cli source (%s...); "+
					"the cli reaches its steps-provided tools (including the verdict tool) over a connection back to this process, which %q severs",
					label, settings.Network, agent.Name, CLISourcePrefix, noNetwork)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// visitAgentSteps calls fn for every step that runs the named agent.
func (c *Config) visitAgentSteps(name string, fn func(label string, step *Step) error) error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Agent != name {
				return nil
			}

			return fn(label, step)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// StepsForAgent returns every step that runs the named agent.
//
// It exists because a step is what an agent's runtime is actually resolved
// AGAINST: image:, network: and the rest merge the agent's own settings with
// the step's overrides (see resolveAgentRuntime), so any question of the form
// "does this agent run in a container" has no answer at the agent alone. A
// caller with no step to hand — preflight for a sub-agent, which no step
// names — gets an empty slice and should fall back to a bare Step.
func (c *Config) StepsForAgent(name string) []Step {
	var steps []Step

	_ = c.visitAgentSteps(name, func(_ string, step *Step) error {
		steps = append(steps, *step)

		return nil
	})

	return steps
}

// AgentRunsOnHost reports whether any step running this agent executes it
// outside a container — i.e. whether this machine needs the agent's own
// binary at all.
//
// An agent named by no step answers true: that is the sub-agent/unused case,
// where there is no step-level override to find and the agent's own settings
// are the whole story.
func (c *Config) AgentRunsOnHost(name string) bool {
	agent, err := c.FindAgent(name)
	if err != nil {
		return true
	}

	steps := c.StepsForAgent(name)
	if len(steps) == 0 {
		return agent.Image == ""
	}

	for _, step := range steps {
		if resolveAgentRuntime(agent, step).Image == "" {
			return true
		}
	}

	return false
}

// checkCLIBinaries reports CLI agents whose executable is not on PATH — the
// same class of check as checkMCPCommands, answered before a run rather than
// at the step that needed it.
//
// A CONTAINERIZED cli agent is exempt: its binary lives in the image, which
// is the entire point of setting image: on it, and demanding the host have
// one too would reject exactly the machine the feature exists for (a CI
// runner with docker and no CLI installed). Whether the image really has the
// binary is a question only docker can answer, so it belongs to preflight
// (see internal/agent's probeCLIImage), not to this microsecond check.
func (c *Config) checkCLIBinaries() []Problem {
	var problems []Problem

	seen := map[string]bool{}

	for _, name := range c.agentNamesInPlans() {
		agent, err := c.FindAgent(name)
		if err != nil || seen[name] || !IsCLISource(agent.Source) {
			continue
		}

		seen[name] = true

		if !c.AgentRunsOnHost(name) {
			continue
		}

		target, err := resolveCLITarget(agent.Source)
		if err != nil {
			continue // already a load error (see validateAgentProviders)
		}

		binary := CLIBinary(target.CLI)

		_, err = exec.LookPath(binary)
		if err != nil {
			problems = append(problems, Problem{
				Target: fmt.Sprintf("agent %q", name),
				Detail: fmt.Sprintf("cli %q not found on PATH (source.model %q)", binary, agent.Source.Model),
			})
		}
	}

	return problems
}
