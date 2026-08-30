package pipeline

import (
	"strings"
	"testing"
)

// TestWithAnswersRefusesTheWholeSetOnOneBadSeed: a seed nobody can read is a
// question that parks in the middle of an unattended run, which is exactly
// what the flag was set to prevent. Refusing at the flag is the only place
// somebody is still there to fix the typo.
func TestWithAnswersRefusesTheWholeSetOnOneBadSeed(t *testing.T) {
	t.Parallel()

	_, err := WithAnswers(t.Context(), []string{"which bump=minor", "no separator here"})
	if err == nil {
		t.Fatal("a malformed --answer was accepted")
	}

	if !strings.Contains(err.Error(), "no separator here") {
		t.Errorf("error = %q, want it to name the value that could not be read", err)
	}
}

// TestWithAnswersPassesThroughWhenEmpty: no flag, no context value, nothing
// for the tool to consult.
func TestWithAnswersPassesThroughWhenEmpty(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	got, err := WithAnswers(ctx, nil)
	if err != nil {
		t.Fatalf("WithAnswers: %v", err)
	}

	if got != ctx {
		t.Error("an empty --answer set changed the context")
	}

	_, err = WithAnswers(ctx, []string{"which bump=minor"})
	if err != nil {
		t.Fatalf("a well-formed seed was refused: %v", err)
	}
}
