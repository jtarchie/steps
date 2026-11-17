package config

// Where a pipeline would run a command on a host rather than in a container:
// what `steps validate --require-image` refuses.
//
// It answers "what runs outside an image:", not "what is safe": a sandbox:
// fences a host command and is still a host command, and a tagged step runs
// on its worker's host. The places it finds are every one a pipeline can name:
// a task's run:, an agent's tools (and its sub-agents', which share its
// container), a shell resource type's check/in/out, a github-* get that
// clones with git, and a stdio MCP server's command. The rest of steps'
// built-ins (expr:, cron, webhook, the other github-* uses, a Concourse
// resource image) run inside this process or an image already.

import (
	"fmt"
	"slices"
	"strings"
)

// HostCommand is one place a pipeline runs a command on a host.
type HostCommand struct {
	// Name is what an allowlist names to accept it: the task, the agent, the resource or the mcp server.
	Name string
	// Where is the place, as validation errors name it.
	Where string
	// Why is what runs there, and what would move it into a container.
	Why string
	// Sandboxed is a host command under sandbox: true, which a fence policy counts where the OS sandbox exists.
	Sandboxed bool
}

// HostCommands lists every place this pipeline runs a command on a host: resources, then mcp servers, then each job's steps, hooks included, in plan order.
func (c *Config) HostCommands() []HostCommand {
	found := c.hostResources()

	for _, server := range c.MCPServers {
		if server.Command != "" {
			found = append(found, HostCommand{
				Name: server.Name, Where: fmt.Sprintf("mcp_servers %q", server.Name),
				Why: "a stdio server: its command runs on the host, and an mcp server has no image:",
			})
		}
	}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(label string, step *Step) error {
			found = append(found, c.hostStep(label, step)...)

			return nil
		})
	}

	return found
}

func (c *Config) hostResources() []HostCommand {
	var found []HostCommand

	for _, resource := range c.Resources {
		resourceType, err := c.FindResourceType(resource.Type)
		if err != nil {
			continue
		}

		where := fmt.Sprintf("resource %q", resource.Name)

		switch resourceType.Config.Backend() {
		case BackendShell:
			if resourceType.Image == "" {
				found = append(found, HostCommand{
					Name: resource.Name, Where: where,
					Why:       fmt.Sprintf("type %q runs check/in/out on the host; give the type an image: (a built-in one, by copying it into resource_types: under another name)", resource.Type) + sandboxed(resourceType.Sandbox),
					Sandboxed: resourceType.Sandbox,
				})
			}
		case BackendGitHub:
			if githubClonesWithGit(resourceType.Config.GitHub, resource.Source) {
				found = append(found, HostCommand{Name: resource.Name, Where: where, Why: "checkout: git runs the git binary on the host"})
			}
		case BackendMCP, BackendExpr, BackendWebhook, BackendCron, BackendConcourse, BackendRegistry:
		}
	}

	return found
}

// githubClonesWithGit reports whether a github-* get runs git: only checkout: git does, and only on the two types that lay a pull request down.
func githubClonesWithGit(kind string, source map[string]any) bool {
	switch kind {
	case GitHubPRsType:
		parsed, err := ParseGitHubPRsSource(source)

		return err == nil && parsed.Checkout == GitHubCheckoutGit
	case GitHubCommentsType:
		parsed, _, err := ParseGitHubCommentsSource(source)

		return err == nil && parsed.Checkout == GitHubCheckoutGit
	}

	return false
}

func (c *Config) hostStep(label string, step *Step) []HostCommand {
	switch kind, _ := step.Kind(); kind {
	case StepKindTask:
		task, err := c.ResolveTask(*step)
		if err != nil || task.Image != "" {
			return nil
		}

		why := "run: runs on the host; set image:"
		if task.Fix != nil {
			why = "run: and its fix: agent's commands run on the host; set image:"
		}

		return []HostCommand{{Name: step.Task, Where: fmt.Sprintf("task %q in %s", step.Task, label), Why: why + sandboxed(task.Sandbox), Sandboxed: task.Sandbox}}
	case StepKindAgent:
		invocation, err := c.ResolveAgentInvocation(*step)
		if err != nil {
			return nil
		}

		return c.hostAgent(fmt.Sprintf("agent %q in %s", step.Agent, label), invocation)
	case StepKindGet, StepKindPut, StepKindTry, StepKindInParallel, StepKindRace, StepKindEnsemble, StepKindLoadVar, StepKindApproval, StepKindDo:
	}

	return nil
}

// hostAgent is an agent whose tools touch the host's files or run a command there, named at where.
func (c *Config) hostAgent(where string, invocation ResolvedInvocation) []HostCommand {
	if invocation.Image != "" {
		return nil
	}

	tools := c.hostTools(invocation.ToolSpecs, map[string]bool{invocation.AgentName: true})
	if len(tools) == 0 {
		return nil
	}

	// A host sub-agent builds its own runner from its own entry, so this agent's fence does not reach it.
	fenced := invocation.Sandbox && !c.unfencedSubAgent(invocation.ToolSpecs, map[string]bool{invocation.AgentName: true})

	return []HostCommand{{
		Name: invocation.AgentName, Where: where,
		Why:       fmt.Sprintf("its tools run on the host (%s); set image: on the agent", strings.Join(tools, ", ")) + sandboxed(fenced),
		Sandboxed: fenced,
	}}
}

// hostTools names the tools in specs that touch the host's files or run a command there, following each sub-agent, which runs where its parent does. No grant is the default file tools.
func (c *Config) hostTools(specs []ToolSpec, seen map[string]bool) []string {
	if len(specs) == 0 {
		specs = DefaultAgentToolSpecs()
	}

	var tools []string

	for _, spec := range specs {
		if spec.Agent != "" && !seen[spec.Agent] {
			seen[spec.Agent] = true

			sub, err := c.FindAgent(spec.Agent)
			if err == nil && len(c.hostTools(sub.Tools, seen)) > 0 {
				tools = append(tools, "sub-agent "+spec.Agent)
			}

			continue
		}

		if spec.Run != "" || (spec.Builtin != "" && ReachesTree([]ToolSpec{spec})) {
			tools = append(tools, ToolSpecName(spec))
		}
	}

	return slices.Compact(tools)
}

// unfencedSubAgent reports a sub-agent reachable from specs that runs host tools under no sandbox: of its own.
func (c *Config) unfencedSubAgent(specs []ToolSpec, seen map[string]bool) bool {
	for _, spec := range specs {
		if spec.Agent == "" || seen[spec.Agent] {
			continue
		}

		seen[spec.Agent] = true

		sub, err := c.FindAgent(spec.Agent)
		if err != nil || sub.Image != "" {
			continue
		}

		if !sub.Sandbox && len(c.hostTools(sub.Tools, map[string]bool{spec.Agent: true})) > 0 {
			return true
		}

		if c.unfencedSubAgent(sub.Tools, seen) {
			return true
		}
	}

	return false
}

// sandboxed says so of a fenced host command, which is fenced and still on the host.
func sandboxed(on bool) string {
	if on {
		return " (sandbox: fences it, and it is still the host)"
	}

	return ""
}
