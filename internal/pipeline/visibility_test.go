package pipeline

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// Why a chain stopped being cacheable is real, documented behavior that used
// to produce no output at all — the user saw steps re-running and had to infer
// the rule from the source.
func TestUnskippableReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		step config.Step
		want string
	}{
		{name: "put", step: config.Step{Put: "release"}, want: "put step"},
		{name: "agent", step: config.Step{Agent: "reviewer"}, want: "agent step"},
		{name: "when", step: config.Step{Task: "t", When: &config.WhenSpec{Run: "true"}}, want: "when: guard"},
		{name: "to", step: config.Step{Task: "t", To: map[string]string{"success": "next"}}, want: "to: routing"},
		{name: "plain task", step: config.Step{Task: "t", Run: "true"}, want: ""},
		{name: "get", step: config.Step{Get: "repo"}, want: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := unskippableReason(test.step)
			if got != test.want {
				t.Errorf("unskippableReason() = %q, want %q", got, test.want)
			}
		})
	}
}

// A when: guard or a to: route disables caching for the whole rest of the
// chain, so the reason is reported by the step that first does it.
func TestFoldStepUnskippableReportsOnce(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}
	step := config.Step{Task: "t", To: map[string]string{"success": "next"}}

	unskippable, err := foldStepUnskippable(cfg, step, false)
	if err != nil {
		t.Fatal(err)
	}

	if !unskippable {
		t.Error("a to: step must make its chain unskippable")
	}

	// Already unskippable: still unskippable, and nothing new to announce.
	unskippable, err = foldStepUnskippable(cfg, step, true)
	if err != nil {
		t.Fatal(err)
	}

	if !unskippable {
		t.Error("an already-unskippable chain must stay unskippable")
	}
}

// A plain task leaves the chain cacheable, so caching keeps working for the
// pipelines that never touch these features.
func TestFoldStepUnskippableLeavesPlainTasksCacheable(t *testing.T) {
	t.Parallel()

	unskippable, err := foldStepUnskippable(&config.Config{}, config.Step{Task: "t", Run: "true"}, false)
	if err != nil {
		t.Fatal(err)
	}

	if unskippable {
		t.Error("a plain task must not make its chain unskippable")
	}
}
