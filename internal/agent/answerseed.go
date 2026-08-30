package agent

// Pre-seeded answers: the --answer flag, which answers a question before
// anybody is asked.
//
// It is a supported production affordance, not a test seam — an unattended run
// that knows the answers in advance is a real thing to want, and a seam only
// the test corpus used would mean the corpus exercised a path production never
// takes.

import (
	"context"
	"fmt"
	"strings"
)

// AnswerSeed is one --answer: a substring of a question, and the answer to
// give when a question contains it.
//
// Matched on the question's TEXT rather than on a position or an id, because
// neither exists when the flag is written: an id is minted by the run that is
// about to start, and which question a model asks first is the model's choice.
// The text is the only part of a question the pipeline author can predict.
type AnswerSeed struct {
	Match  string
	Answer string
}

// answerSeedsKey types the context value carrying a run's seeds.
type answerSeedsKey struct{}

// WithAnswerSeeds attaches pre-seeded answers to a run's context.
func WithAnswerSeeds(ctx context.Context, seeds []AnswerSeed) context.Context {
	if len(seeds) == 0 {
		return ctx
	}

	return context.WithValue(ctx, answerSeedsKey{}, seeds)
}

// ParseAnswerSeed parses one --answer value, written `substring=answer`.
//
// Split on the FIRST `=`, so an answer may contain one; a match may not, which
// is the right way round — the match is a fragment of a question somebody
// wrote, and an answer is arbitrary text.
func ParseAnswerSeed(raw string) (AnswerSeed, error) {
	match, answer, found := strings.Cut(raw, "=")
	if !found {
		return AnswerSeed{}, fmt.Errorf("--answer %q must be written as <question substring>=<answer>", raw)
	}

	match, answer = strings.TrimSpace(match), strings.TrimSpace(answer)
	if match == "" || answer == "" {
		return AnswerSeed{}, fmt.Errorf("--answer %q needs both a question substring and an answer", raw)
	}

	return AnswerSeed{Match: match, Answer: answer}, nil
}

// matchAnswerSeed returns the answer seeded for this question, if one matches.
//
// Case-insensitive substring, first seed wins. A seed is deliberately NOT
// consumed by matching it: an across: matrix asks the same question from
// twelve cells and an attempts: restart asks it again, and a seed that
// evaporated after the first would answer one of them and park the rest.
func matchAnswerSeed(ctx context.Context, question string) (string, bool) {
	seeds, _ := ctx.Value(answerSeedsKey{}).([]AnswerSeed)

	lowered := strings.ToLower(question)

	for _, seed := range seeds {
		if strings.Contains(lowered, strings.ToLower(seed.Match)) {
			return seed.Answer, true
		}
	}

	return "", false
}
