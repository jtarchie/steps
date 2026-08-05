package config

// The approval: step — pause a plan and wait for a human.

import "fmt"

// Approval pauses a plan until a person says yes.
//
// Agent pipelines eventually gate something that cannot be undone —
// publishing, deploying, sending — and this is the oldest and most direct
// safeguard for that. Before it, a plan ran start to finish with no place to
// stop and ask.
type Approval struct {
	// Message is what the waiting approval says it is asking about. Required:
	// an approval prompt with no question is one nobody can answer
	// responsibly, and "approve job build step 3?" is not a question.
	Message string `yaml:"message"`
	// Timeout bounds the wait (e.g. "24h"). Empty means the default (see
	// defaultApprovalTimeout in internal/pipeline). An expired approval
	// classifies as ABORTED, not failed — the outcome vocabulary already
	// distinguishes "nobody answered" from "somebody said no", and conflating
	// them would make a silent expiry indistinguishable from a rejection.
	Timeout string `yaml:"timeout,omitempty"`
}

// validateApprovals enforces that an approval asks something answerable and
// waits a sane length of time.
func (c *Config) validateApprovals() error {
	for _, job := range c.Jobs {
		err := job.visitSteps(func(label string, step *Step) error {
			if step.Approval == nil {
				return nil
			}

			if step.Approval.Message == "" {
				return fmt.Errorf("%s: approval.message is required; an approval nobody can read is one nobody can answer responsibly", label)
			}

			if step.Approval.Timeout == "" {
				return nil
			}

			parsed, err := ParseTimeout(step.Approval.Timeout)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("%s: approval.timeout %q is not a positive duration (e.g. 24h)", label, step.Approval.Timeout)
			}

			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
