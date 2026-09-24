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
	"regexp"
	"time"
)

// Problem is one reason a pipeline cannot run here, with the target it
// concerns so a report can be read target-by-target rather than as prose.
type Problem struct {
	// Target names what is wrong, e.g. `agent "coder"` or `mcp "gopls"`.
	Target string
	// Detail says what about it is wrong, and where possible how to fix it.
	Detail string
	// Transient marks a problem that WAITING could fix — a server that did
	// not answer, a token that needs refreshing — as opposed to one no amount
	// of time will change, like a tool the server does not expose or required
	// arguments the pipeline never sends.
	//
	// It exists because the same problem deserves opposite reactions from the
	// two callers. `steps run` refuses either way: a person is waiting, and
	// starting a run that cannot finish wastes their time and money. `steps
	// watch` is a daemon, and quitting because a VPN was not up yet — or
	// because a token needed the refresh the next poll would have done — is
	// how a watcher that should have recovered on its own ends up dead all
	// weekend.
	Transient bool
}

func (p Problem) Error() string { return p.Target + ": " + p.Detail }

// CheckEnvironment reports what this machine is missing for the pipeline to
// run: credentials the agents need, and binaries the MCP servers and CLI
// agents need. It makes no network calls and starts no processes — every
// check here is a yes/no fact answerable in microseconds.
//
// It reports every problem it finds rather than the first, since discovering
// them one run at a time is the failure mode it exists to end.
func (c *Config) CheckEnvironment() []Problem {
	problems := c.checkAgentCredentials()
	problems = append(problems, c.checkResourceCredentials()...)
	problems = append(problems, c.checkMCPServers()...)

	return append(problems, c.checkCLIBinaries()...)
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

		target, err := resolveAgentTarget(agent.Source)
		if err != nil || !target.RequiresKey || target.APIKeyEnv == "" {
			// An unresolvable target is already a load error (see
			// validateAgentProviders); a provider that needs no key has
			// nothing to check.
			continue
		}

		if value, ok := os.LookupEnv(target.APIKeyEnv); !ok || value == "" {
			problems = append(problems, Problem{
				Target: fmt.Sprintf("agent %q", name),
				Detail: fmt.Sprintf("$%s is not set (source.api_key_env)", target.APIKeyEnv),
			})
		}
	}

	return problems
}

// literalEnvCall matches env("NAME") with a literal name. A computed name
// (`env(source.token_env ?? "X")`) is skipped: which variable it reads is a
// runtime answer, and guessing would report a key the resource never asks for.
var literalEnvCall = regexp.MustCompile(`\benv\(\s*"([A-Za-z_][A-Za-z0-9_]*)"\s*\)`)

// checkResourceCredentials reports expr-backed resources whose expressions
// read an unset variable by literal name. Without it the first sign is a
// `steps web` poll failing into the daemon log, every interval, with nothing
// red in the UI.
//
// It reads the env() calls rather than the type's env: list because that list
// is an allow-list, not a requirement: git's SSH_AUTH_SOCK is optional, and
// demanding it would refuse every https pipeline. Shell backends are skipped
// for the same reason: an unset $VAR in a script may be deliberate.
func (c *Config) checkResourceCredentials() []Problem {
	var problems []Problem

	seen := map[string]bool{}

	for _, job := range c.Jobs {
		_ = job.visitSteps(func(_ string, step *Step) error {
			name, ok := step.resourceName()
			if !ok || seen[name] {
				return nil
			}

			seen[name] = true
			problems = append(problems, c.resourceCredentialProblems(name)...)

			return nil
		})
	}

	return problems
}

func (c *Config) resourceCredentialProblems(name string) []Problem {
	resource, err := c.FindResource(name)
	if err != nil {
		return nil // an unresolvable resource is already a load error
	}

	resourceType, err := c.FindResourceType(resource.Type)
	if err != nil || resourceType.Config.Backend() != BackendExpr {
		return nil
	}

	var problems []Problem

	reported := map[string]bool{}
	expression := resourceType.Config.Expr

	for _, src := range []string{expression.Check, expression.In, expression.Out} {
		for _, match := range literalEnvCall.FindAllStringSubmatch(src, -1) {
			variable := match[1]
			if reported[variable] {
				continue
			}

			reported[variable] = true

			if value, ok := os.LookupEnv(variable); !ok || value == "" {
				problems = append(problems, Problem{
					Target: fmt.Sprintf("resource %q", name),
					Detail: fmt.Sprintf("$%s is not set (read by resource_type %q)", variable, resourceType.Name),
				})
			}
		}
	}

	return problems
}

// checkMCPServers reports every mcp_servers: entry this machine cannot satisfy without a request: a stdio binary that is not on PATH, and a bearer credential whose api_key_env names an unset variable. The first would have caught a `gopls` grant on a machine without gopls installed; the second is the same treatment checkAgentCredentials gives an agent's api_key_env, and it was missing — an unset $GITHUB_PAT was knowable in microseconds and surfaced only as a failed run.
//
// An oauth server is deliberately not checked here: its credential is a token file internal/mcp owns, and this package may not import it. It is checked where it is spent, and reported by whoever holds it — see the web UI's mcp tab.
func (c *Config) checkMCPServers() []Problem {
	var problems []Problem

	for _, server := range c.MCPServers {
		status := server.StaticStatus()
		if status.Readiness != MCPMissing {
			continue
		}

		problems = append(problems, Problem{
			Target: fmt.Sprintf("mcp %q", server.Name),
			Detail: status.Detail,
		})
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

		_, err = resolveAgentTarget(agent.Source)
		if err != nil {
			return fmt.Errorf("agent %q: %w", name, err)
		}

		// A fallback nobody can resolve is a fallback that will not save you,
		// discovered during the outage it exists for.
		for i := range agent.Fallback {
			_, err = resolveAgentTarget(agent.Fallback[i].Source)
			if err != nil {
				return fmt.Errorf("agent %q fallback[%d]: %w", name, i, err)
			}
		}
	}

	return nil
}

// Preflight tunes the pre-run health check that proves a job's models and MCP
// servers work before any of its steps run. See internal/agent's Preflight.
type Preflight struct {
	// Disabled turns the check off entirely. Spelled as the negative so the
	// zero value — an absent preflight: block — is ON, which is the useful
	// default: the check exists because nobody thinks to enable it until
	// after it would have saved them.
	Disabled bool `yaml:"disabled,omitempty"`
	// Timeout bounds one check. A model that has not answered in this long is
	// reported as unavailable rather than waited on.
	Timeout string `yaml:"timeout,omitempty"`
	// Cache is how long a verified target is trusted for. It is a real
	// requirement, not an afterthought: without it every `steps web` poll
	// pays for a probe request against every model in the pipeline.
	Cache string `yaml:"cache,omitempty"`
}

// Default preflight settings, applied when the pipeline sets none.
const (
	defaultPreflightTimeout = 30 * time.Second
	defaultPreflightCache   = 5 * time.Minute
)

// PinLifetime is how long an agent stays on a fallback with nothing
// re-affirming that its primary is still unwell. internal/agent owns the
// behavior and documents the reasoning at length; the value lives here so
// validatePreflight can hold defaults.preflight.cache: below it, which is a
// load-time question about a setting rather than a runtime one.
const PinLifetime = 15 * time.Minute

// Enabled reports whether preflight runs at all. A nil Preflight (no
// defaults: block) is enabled.
func (p *Preflight) Enabled() bool {
	return p == nil || !p.Disabled
}

// PreflightSettings is this pipeline's preflight block, or nil when it has
// none — which every accessor above treats as the default.
//
// A method rather than two callers each reaching through `cfg.Defaults`: that
// field is a pointer and is nil for any pipeline without a defaults: block, so
// the reach is a nil dereference waiting for the first pipeline that does not
// write one.
func (c *Config) PreflightSettings() *Preflight {
	if c.Defaults == nil {
		return nil
	}

	return c.Defaults.Preflight
}

// ProbeTimeout is the per-check deadline.
func (p *Preflight) ProbeTimeout() time.Duration {
	return p.duration(func(pre *Preflight) string { return pre.Timeout }, defaultPreflightTimeout)
}

// CacheWindow is how long a verified target stays trusted.
func (p *Preflight) CacheWindow() time.Duration {
	return p.duration(func(pre *Preflight) string { return pre.Cache }, defaultPreflightCache)
}

// duration reads one of the two duration fields, falling back to its default.
// A malformed value cannot reach here — validatePreflight rejects it at load —
// so an unexpected parse failure takes the default rather than failing a run
// over a setting about how eagerly to check things.
func (p *Preflight) duration(get func(*Preflight) string, fallback time.Duration) time.Duration {
	if p == nil {
		return fallback
	}

	raw := get(p)
	if raw == "" {
		return fallback
	}

	parsed, err := ParseTimeout(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}

	return parsed
}

// validatePreflight rejects a malformed preflight duration at load rather than
// silently ignoring it, since a typo'd timeout would otherwise read as "this
// is configured" while doing nothing.
func (c *Config) validatePreflight() error {
	if c.Defaults == nil || c.Defaults.Preflight == nil {
		return nil
	}

	for _, field := range []struct{ name, value string }{
		{"timeout", c.Defaults.Preflight.Timeout},
		{"cache", c.Defaults.Preflight.Cache},
	} {
		if field.value == "" {
			continue
		}

		parsed, err := ParseTimeout(field.value)
		if err != nil || parsed <= 0 {
			return fmt.Errorf("defaults.preflight.%s: %q is not a positive duration (e.g. 30s, 5m)", field.name, field.value)
		}

		// A cache window longer than the pin lifetime makes the lifetime a
		// number the pipeline no longer obeys. Freshness is measured against
		// the pin, so every pass inside such a window reads answers that
		// predate it, cannot release on them, and renews the lifetime anyway
		// — the real floor becomes the cache window, not the fifteen minutes
		// the constant advertises. Refused at load, where a setting is still
		// a setting.
		if field.name == "cache" && parsed > PinLifetime {
			return fmt.Errorf(
				"defaults.preflight.cache: %q is longer than the %s failover pin lifetime, which would keep an agent on a fallback past its own expiry",
				field.value, PinLifetime,
			)
		}
	}

	return nil
}
