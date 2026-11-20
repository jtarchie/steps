package config

// The ask: step: the pipeline asks a person a question.

import (
	"fmt"
	"slices"
	"strings"
)

// validateAsks refuses an ask: nobody could answer, or whose answer could not be named.
func (c *Config) validateAsks() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(validateAskStep)
		if err != nil {
			return err
		}
	}

	return nil
}

func validateAskStep(label string, step *Step) error {
	if step.Ask == "" {
		return refuseAskFields(label, step)
	}

	// Both the capture's root and the artifact's directory, so the narrower of the two spellings.
	if strings.ContainsFunc(step.Ask, func(r rune) bool { return r == '.' || !validNameRune(r) }) {
		return fmt.Errorf("%s: ask %q must contain only letters, digits, %q or %q: it names the answer's artifact and its ((.:name))", label, step.Ask, "_", "-")
	}

	if strings.TrimSpace(step.Question) == "" {
		return fmt.Errorf("%s: ask %q needs a question:; a park nobody can read is one nobody can answer", label, step.Ask)
	}

	return validateAskOptions(label, step)
}

func validateAskOptions(label string, step *Step) error {
	for i, option := range step.Options {
		if strings.TrimSpace(option) == "" {
			return fmt.Errorf("%s: ask %q options[%d] is empty; nobody can choose it", label, step.Ask, i)
		}

		if slices.Contains(step.Options[:i], option) {
			return fmt.Errorf("%s: ask %q offers %q twice", label, step.Ask, option)
		}
	}

	if step.Default != "" && len(step.Options) > 0 && !slices.Contains(step.Options, step.Default) {
		return fmt.Errorf("%s: ask %q default %q is not one of its options: %s", label, step.Ask, step.Default, strings.Join(step.Options, ", "))
	}

	return nil
}

func refuseAskFields(label string, step *Step) error {
	switch {
	case step.Question != "":
		return fmt.Errorf("%s: question: without ask: has no meaning here", label)
	case len(step.Options) > 0:
		return fmt.Errorf("%s: options: without ask: has no meaning here", label)
	case step.Default != "":
		return fmt.Errorf("%s: default: without ask: has no meaning here", label)
	}

	return nil
}
