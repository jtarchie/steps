package pipeline

// Pre-seeded answers, on their way from the command line to the ask_user
// tool.
//
// This file is a pass-through and deliberately nothing more: the seeds are
// parsed and matched in internal/agent, next to the tool that consumes them,
// and main reaches that package only through here — the same shape as
// WithArtifactStore.

import (
	"context"

	"github.com/jtarchie/steps/internal/agent"
)

// WithAnswers attaches --answer seeds to a run's context, parsing each one
// and refusing the whole set if any is malformed. A seed nobody can read is a
// question that parks in the middle of an unattended run, which is exactly
// what the flag was set to prevent.
func WithAnswers(ctx context.Context, raw []string) (context.Context, error) {
	if len(raw) == 0 {
		return ctx, nil
	}

	seeds := make([]agent.AnswerSeed, 0, len(raw))

	for _, entry := range raw {
		seed, err := agent.ParseAnswerSeed(entry)
		if err != nil {
			return ctx, err //nolint:wrapcheck // ParseAnswerSeed's error already names the flag and the value
		}

		seeds = append(seeds, seed)
	}

	return agent.WithAnswerSeeds(ctx, seeds), nil
}
