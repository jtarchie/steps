package pipeline

import (
	"strings"
	"testing"
)

// TestParseAnswersRefusesTheWholeSetOnOneBadSeed: a seed nobody can read is a
// question that parks in the middle of an unattended run, which is exactly
// what the flag was set to prevent. Refusing at the flag is the only place
// somebody is still there to fix the typo.
func TestParseAnswersRefusesTheWholeSetOnOneBadSeed(t *testing.T) {
	t.Parallel()

	_, err := ParseAnswers([]string{"which bump=minor", "no separator here"})
	if err == nil {
		t.Fatal("a malformed --answer was accepted")
	}

	if !strings.Contains(err.Error(), "no separator here") {
		t.Errorf("error = %q, want it to name the value that could not be read", err)
	}
}

// TestParseAnswersPassesThroughWhenEmpty: no flag, no seeds, nothing for the
// tool to consult.
func TestParseAnswersPassesThroughWhenEmpty(t *testing.T) {
	t.Parallel()

	got, err := ParseAnswers(nil)
	if err != nil || got != nil {
		t.Fatalf("ParseAnswers(nil) = %v, %v; want nil, nil", got, err)
	}

	got, err = ParseAnswers([]string{"which bump=minor"})
	if err != nil || len(got) != 1 {
		t.Fatalf("a well-formed seed was refused: %v, %v", got, err)
	}
}
