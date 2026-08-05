package config

// The checks that answer "can this pipeline actually run HERE?" — as opposed
// to "is this YAML internally consistent?", which is what the rest of this
// package's validators answer.
//
// The split matters. `steps validate` used to report ok for a pipeline with a
// misspelled provider prefix, an unset API key, and an MCP server whose binary
// was not installed — three fatal problems, all knowable in milliseconds, none
// of them checked until a run was already underway and (for an agent step)
// already billing. A junior user reasonably reads `ok` as "this will run";
// today it means "the YAML parses".
//
// Environment-dependent checks deliberately do NOT run at LoadConfig. A key
// being absent is a fact about this machine right now, not about the file, and
// making it a load error would break `steps plan` on a laptop, every test that
// never sets a key, and any CI job that lints a pipeline it does not run.

import (
	"fmt"
	"os"
	"os/exec"
)

// Problem is one reason a pipeline cannot run here, with the target it
// concerns so a report can be read target-by-target rather than as prose.
type Problem struct {
	// Target names what is wrong, e.g. `agent "coder"` or `mcp "gopls"`.
	Target string
	// Detail says what about it is wrong, and where possible how to fix it.
	Detail string
}

func (p Problem) Error() string { return p.Target + ": " + p.Detail }

// CheckEnvironment reports what this machine is missing for the pipeline to
// run: credentials the agents need, and binaries the MCP servers need. It
// makes no network calls and starts no processes — every check here is a
// yes/no fact answerable in microseconds.
//
// It reports every problem it finds rather than the first, since discovering
// them one run at a time is the failure mode it exists to end.
func (c *Config) CheckEnvironment() []Problem {
	problems := c.checkAgentCredentials()

	return append(problems, c.checkMCPCommands()...)
}

// checkAgentCredentials reports agents whose api_key_env names an unset
// variable. Only agents a step actually names are checked: built-in profiles
// are always registered and mostly unused, so demanding credentials for all of
// them would report problems no run would ever hit.
func (c *Config) checkAgentCredentials() []Problem {
	var problems []Problem

	seen := map[string]bool{}

	for _, name := range c.agentNamesInPlans() {
		agent, err := c.FindAgent(name)
		if err != nil || seen[name] {
			continue
		}

		seen[name] = true

		_, _, apiKeyEnv, requiresKey, _, err := resolveAgentTarget(agent.Source)
		if err != nil || !requiresKey || apiKeyEnv == "" {
			// An unresolvable target is already a load error (see
			// validateAgentProviders); a provider that needs no key has
			// nothing to check.
			continue
		}

		if value, ok := os.LookupEnv(apiKeyEnv); !ok || value == "" {
			problems = append(problems, Problem{
				Target: fmt.Sprintf("agent %q", name),
				Detail: fmt.Sprintf("$%s is not set (source.api_key_env)", apiKeyEnv),
			})
		}
	}

	return problems
}

// checkMCPCommands reports stdio MCP servers whose binary is not on PATH.
// This is the check that would have caught a `gopls` grant on a machine
// without gopls installed — discovered, before this existed, at the moment an
// agent step began, which for a long plan is a long way in.
func (c *Config) checkMCPCommands() []Problem {
	var problems []Problem

	for _, server := range c.MCPServers {
		if server.Command == "" {
			continue // an HTTP endpoint; reachability is a live probe, not a PATH lookup
		}

		_, err := exec.LookPath(server.Command)
		if err != nil {
			problems = append(problems, Problem{
				Target: fmt.Sprintf("mcp %q", server.Name),
				Detail: fmt.Sprintf("command %q not found on PATH", server.Command),
			})
		}
	}

	return problems
}

// agentNamesInPlans lists every agent name a job step (or a task's fix:)
// invokes, in plan order.
func (c *Config) agentNamesInPlans() []string {
	var names []string

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			if step.Agent != "" {
				names = append(names, step.Agent)
			}

			if step.Fix != nil && step.Fix.Agent != "" {
				names = append(names, step.Fix.Agent)
			}

			return nil
		})
	}

	return names
}

// validateAgentProviders rejects, at load time, a model whose provider prefix
// this build does not know — the one half of "this cannot run" that is a
// static property of the file rather than of the machine.
//
// It is the check that would have caught `opencoder/...` written for
// `opencode/...`: a typo in a model name should cost a load, not a run, and
// especially not a run that is billed per token.
func (c *Config) validateAgentProviders() error {
	seen := map[string]bool{}

	for _, name := range c.agentNamesInPlans() {
		if seen[name] {
			continue
		}

		seen[name] = true

		agent, err := c.FindAgent(name)
		if err != nil {
			continue // validateStepReferences reports an unknown agent
		}

		_, _, _, _, _, err = resolveAgentTarget(agent.Source)
		if err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}
	}

	return nil
}
