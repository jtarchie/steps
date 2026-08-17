package config

// The pipeline-level defaults: block.

import (
	"fmt"
	"os"
)

// stepsModelEnv names the environment variable that supplies a model when the
// pipeline declares none. It is what makes an example runnable on a machine
// whose available model nobody could have guessed when the example was
// written.
const stepsModelEnv = "STEPS_MODEL"

// Defaults holds pipeline-wide fallbacks. It carries exactly one field, and
// the omissions are deliberate.
//
// model: is pure repetition — every agent in a pipeline usually calls the same
// one, and spelling out a connection string per agent is noise that makes
// examples unrunnable for anyone whose setup differs. Defaulting it erases
// nothing: the resolved model is still hashed, so overriding it correctly
// busts the affected agents' caches.
//
// attempts:/timeout: are deliberately NOT here, upholding the argument in
// docs/attempts-timeout.md. A budget default changes failure behavior at a
// distance: a step written with no timeout means "this one has no deadline",
// and a pipeline-level default silently converts that into a deadline
// somebody else picked. image: is out for the same reason in reverse — an
// absent image *means* "run on the host", so a default would flip a
// documented semantic for every step that never asked.
type Defaults struct {
	// Model is the source.model: an agents: entry inherits when it declares
	// none. Falls back to $STEPS_MODEL when unset here.
	Model string `yaml:"model,omitempty"`
	// Preflight tunes the pre-run health check (see Preflight). It belongs
	// here, unlike attempts:/timeout:, because it changes no step's failure
	// semantics — it only decides how early a failure that WOULD happen is
	// discovered.
	Preflight *Preflight `yaml:"preflight,omitempty"`
	// DelegateBudgetPercent is how much of an agent's REMAINING token
	// allowance one sub-agent call may take, pipeline-wide (see
	// Agent.DelegateBudgetPercent for the per-agent override, and
	// DefaultDelegateBudgetPercent for the value when neither is set).
	DelegateBudgetPercent *int `yaml:"delegate_budget_percent,omitempty"`
	// VersionHistory is how many versions of each resource steps remembers.
	//
	// It belongs to the pipeline because the right number is a property of
	// the resources it watches: a git branch produces a version per push, a
	// chat feed one per message. `--version-history` supplies a default when
	// this is unset, and DefaultVersionHistory when neither is.
	//
	// The floor is not zero: history is what lets a job build a version that
	// scrolled out of a check's window, and what a `passed:` gate proves
	// against — a version pruned from history takes its green record with it,
	// so a number below what a slow downstream job needs holds that job back.
	VersionHistory *int `yaml:"version_history,omitempty"`
	// RunHistory is how many runs of each job steps keeps in full.
	//
	// It belongs next to VersionHistory because it answers the same question
	// about the other half of the database, and it was the half with no answer:
	// resource_versions was the only table with a prune path, so every run
	// recorded its nodes, its events, its usage and its agent transcripts
	// forever. Measured on a pipeline answering Slack mentions overnight, a
	// build cost about 23KB and nothing ever gave a byte back.
	//
	// Per JOB rather than pipeline-wide, for the reason the cap exists at all:
	// a global number makes the least active job the least inspectable, since a
	// busy neighbour evicts its history.
	//
	// Reaping a run takes its events, steps and usage rows with it by foreign
	// key, and the nodes no surviving run refers to with them — so the floor is
	// not free of meaning either. It is a cache horizon: a step whose content
	// has not changed since before the window runs once more instead of being
	// skipped. `--run-history` supplies a default when this is unset, and
	// DefaultRunHistory when neither is.
	RunHistory *int `yaml:"run_history,omitempty"`
}

// DefaultRunHistory is how many runs of each job steps keeps when neither the
// pipeline nor the command line says.
//
// Smaller than DefaultVersionHistory by an order of magnitude, and deliberately:
// a version is a few dozen bytes and a run is tens of kilobytes, so the same
// number would make these two caps mean very different things. A hundred runs
// is far more than anyone scrolls back through while keeping a busy watch's
// database in the low tens of megabytes.
const DefaultRunHistory = 100

// RunHistoryLimit is how many runs of each job to keep, resolving the
// pipeline's own setting against the built-in default.
//
// Zero from the pipeline means NO limit, matching every other cap in this repo,
// and is why this cannot simply be a value with a default: the difference
// between "unset, use the default" and "set to zero, keep everything" is the
// difference between a bounded database and an unbounded one, and only a pointer
// can carry it.
func (c *Config) RunHistoryLimit() int {
	if c.Defaults != nil && c.Defaults.RunHistory != nil {
		return *c.Defaults.RunHistory
	}

	return DefaultRunHistory
}

// DefaultVersionHistory is how many versions of each resource steps
// remembers when neither the pipeline nor the command line says.
//
// Deliberately the same order as the consumed-version bound next to it: far
// past any check window anyone polls, while keeping a busy resource from
// growing the table forever.
const DefaultVersionHistory = 1000

// VersionHistoryLimit is how many versions of each resource to keep,
// resolving the pipeline's own setting against the built-in default.
//
// A caller that has a command-line default writes it into Defaults before
// asking, so precedence stays in one place: whatever the pipeline says wins,
// because it is the thing that knows what its resources do.
// Zero means NO limit, matching the convention docs/attempts-timeout.md states
// for every dial in this repo — omitted takes the default, 0 means no limit. It
// read `> 0` before, which quietly turned `version_history: 0` into the 1000
// default: a reader who wrote it to stop pruning got pruning anyway, and the
// field disagreed with run_history: next to it.
func (c *Config) VersionHistoryLimit() int {
	if c.Defaults != nil && c.Defaults.VersionHistory != nil {
		return *c.Defaults.VersionHistory
	}

	return DefaultVersionHistory
}

// DefaultDelegateBudgetPercent is how much of its remaining allowance an agent
// hands to one sub-agent call when nothing says otherwise.
//
// A fraction of what REMAINS, so delegation cannot drain a parent outright:
// successive calls take a tenth of a shrinking number and the parent always
// keeps something to finish its own work with. Ten percent leaves room for
// roughly a dozen helpers before the slices get small enough to be worth
// noticing, which is far past what any sensible plan asks for.
const DefaultDelegateBudgetPercent = 10

// validateDelegateBudgets rejects a delegation share that is not a
// percentage.
//
// Above 100 a child would be handed more than its parent has left, which is
// the opposite of the bound this exists to create; at or below 0 the whole
// inheritance silently reverts to every agent running unbounded. Both read as
// configuration and bind nothing, which is what this package rejects at load
// everywhere else. The schema says 1..100 too, but nothing enforces a schema
// at run time — `steps run` never reads it.
func (c *Config) validateDelegateBudgets() error {
	if c.Defaults != nil {
		err := validateDelegatePercent("defaults", c.Defaults.DelegateBudgetPercent)
		if err != nil {
			return err
		}
	}

	for _, agent := range c.Agents {
		err := validateDelegatePercent(fmt.Sprintf("agent %q", agent.Name), agent.DelegateBudgetPercent)
		if err != nil {
			return err
		}
	}

	return nil
}

func validateDelegatePercent(label string, percent *int) error {
	if percent == nil {
		return nil
	}

	if *percent < 1 || *percent > 100 {
		return fmt.Errorf("%s: delegate_budget_percent must be between 1 and 100 (it is the share of an agent's REMAINING allowance one sub-agent call may take; omit it for the default of %d)",
			label, DefaultDelegateBudgetPercent)
	}

	return nil
}

// DelegateBudgetFraction is the share of the named agent's remaining
// allowance that one of its sub-agent calls may take: the agent's own
// override, else the pipeline default, else DefaultDelegateBudgetPercent.
//
// Keyed by name rather than by *Agent because every caller has the name and
// an unresolvable one is not worth an error here — it means the agent could
// not have run at all, which the step reports far more clearly.
func (c *Config) DelegateBudgetFraction(agentName string) float64 {
	percent := DefaultDelegateBudgetPercent

	// defaults: is optional, so the block is nil on most pipelines.
	if c.Defaults != nil && c.Defaults.DelegateBudgetPercent != nil {
		percent = *c.Defaults.DelegateBudgetPercent
	}

	agent, err := c.FindAgent(agentName)
	if err == nil && agent.DelegateBudgetPercent != nil {
		percent = *agent.DelegateBudgetPercent
	}

	return float64(percent) / 100
}

// validateAgentModels rejects a step whose agent ends up with no model.
//
// It checks only agents a step actually names: built-in profiles are always
// registered and mostly unused, and demanding a model from all of them would
// fail every pipeline that touches none.
//
// Without this the failure surfaced at run time, after planning had begun, as
// `model "" has no known provider prefix; set source.endpoint` — which names
// the wrong fix. Nothing is wrong with the endpoint; there is no model.
func (c *Config) validateAgentModels() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			name := step.Agent
			if name == "" && step.Fix != nil {
				name = step.Fix.Agent
			}

			if name == "" {
				return nil
			}

			agent, findErr := c.FindAgent(name)
			if findErr != nil {
				return nil //nolint:nilerr // validateStepReferences reports an unknown agent
			}

			if agent.Source.Model == "" {
				return fmt.Errorf("%s: agent %q has no model; set source.model on it, a pipeline-level defaults.model, or $%s",
					label, name, stepsModelEnv)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}

// applyDefaults fills in each agent's unset source.model from $STEPS_MODEL or
// the pipeline's defaults: block.
//
// Resolution order is agent → $STEPS_MODEL → defaults:.
//
// An agent naming its own model always wins: that is a deliberate per-agent
// decision ("this one needs the big model"), and nothing should override it.
// Between the other two, the environment wins, because defaults.model is the
// pipeline's *suggestion* and it cannot know what the reader actually runs —
// a checked-in model name is why a shipped example is otherwise unrunnable by
// anyone but its author. STEPS_MODEL=... makes it run without editing a file.
//
// It runs after built-in registration, so a bare `@builtin/reviewer`
// reference becomes usable with no agents: entry at all.
func (c *Config) applyDefaults() {
	model := os.Getenv(stepsModelEnv)

	if model == "" && c.Defaults != nil {
		model = c.Defaults.Model
	}

	if model == "" {
		return
	}

	for i := range c.Agents {
		if c.Agents[i].Source.Model == "" {
			c.Agents[i].Source.Model = model
		}
	}
}
