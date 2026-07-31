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

// applyDefaults fills in each agent's unset source.model from the pipeline's
// defaults: block, or from $STEPS_MODEL.
//
// Resolution order is agent → defaults: → environment, most specific first,
// so a pipeline can pin one agent to a bigger model while the rest follow the
// default. It runs after built-in registration, so a bare `@builtin/reviewer`
// reference becomes usable with no agents: entry at all.
func (c *Config) applyDefaults() {
	model := ""
	if c.Defaults != nil {
		model = c.Defaults.Model
	}

	if model == "" {
		model = os.Getenv(stepsModelEnv)
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
