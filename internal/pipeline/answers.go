package pipeline

// Pre-seeded answers, on their way from the command line to the ask_user
// tool. Parsed and matched in internal/agent, next to the tool that consumes
// them; the CLI reaches that package only through here.

import (
	"github.com/jtarchie/steps/internal/agent"
)

// AnswerSeed is one parsed --answer.
type AnswerSeed = agent.AnswerSeed

// ParseAnswers parses --answer seeds for Run.Answers, refusing the whole set
// if any is malformed. A seed nobody can read is a question that parks in the
// middle of an unattended run, which is exactly what the flag was set to
// prevent.
func ParseAnswers(raw []string) ([]AnswerSeed, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	seeds := make([]AnswerSeed, 0, len(raw))

	for _, entry := range raw {
		seed, err := agent.ParseAnswerSeed(entry)
		if err != nil {
			return nil, err //nolint:wrapcheck // ParseAnswerSeed's error already names the flag and the value
		}

		seeds = append(seeds, seed)
	}

	return seeds, nil
}
