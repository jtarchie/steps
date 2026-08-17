package config

import (
	"strings"
	"testing"
)

// TestContextWindowsAreOrderedMostSpecificFirst enforces the invariant the
// table states in prose and nothing else checked.
//
// contextWindowFor returns the FIRST fragment that is a substring of the model
// name, so a fragment that contains another one has to come first: every model
// matching `claude-sonnet-4-5` also matches `claude`, and if the family entry
// came first the specific one could never win. The table is right today, but
// it is hand-maintained, roughly tripled in this change, and the ordering is
// load-bearing for which context window a real model gets — an entry appended
// in the natural-looking place silently re-budgets whatever it shadows, with
// no compiler or test to say so. This is that test.
func TestContextWindowsAreOrderedMostSpecificFirst(t *testing.T) {
	t.Parallel()

	for i, general := range contextWindows {
		for j, specific := range contextWindows[i+1:] {
			if general.fragment == specific.fragment {
				t.Errorf("contextWindows has %q twice (entries %d and %d); the second can never be reached",
					general.fragment, i, i+1+j)

				continue
			}

			// specific contains general, so anything matching specific also
			// matches general — and general, being earlier, wins first.
			if strings.Contains(specific.fragment, general.fragment) {
				t.Errorf("contextWindows entry %d (%q, %d tokens) is unreachable: entry %d (%q, %d tokens) is a substring of it and comes first, so it matches every model %q would have",
					i+1+j, specific.fragment, specific.window,
					i, general.fragment, general.window,
					specific.fragment)
			}
		}
	}
}

// TestContextWindowForKnownFamilies spot-checks the shadowing the ordering
// test above protects: a specific model must beat its own family entry, and a
// family entry must still catch a sibling the table does not enumerate.
func TestContextWindowForKnownFamilies(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model  string
		want   int
		wantOK bool
	}{
		{"anthropic/claude-sonnet-4-5", 1_000_000, true},
		{"anthropic/claude-opus-4-5", 200_000, true},
		{"openrouter/anthropic/claude-sonnet-4.5", 1_000_000, true},
		{"anthropic/claude-opus-5[1m]", 1_000_000, true},
		{"openai/gpt-5.4-mini", 400_000, true},
		{"openai/gpt-5.4", 1_050_000, true},
		{"openai/gpt-5", 400_000, true},
		{"opencode/glm-5.2", 1_000_000, true},
		{"opencode/glm-5.1", 200_000, true},
		// Unrecognized: the conservative assumption, reported as NOT derived so
		// the compaction log can say "assumed" rather than name a window.
		{"lmstudio/some-local-build", defaultContextWindow, false},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			got, ok := contextWindowFor(test.model)
			if got != test.want || ok != test.wantOK {
				t.Errorf("contextWindowFor(%q) = (%d, %v), want (%d, %v)",
					test.model, got, ok, test.want, test.wantOK)
			}
		})
	}
}

// TestResolveMaxContextBytesPrecedence pins step-over-agent-over-default.
//
// The step wins because context_paths: is itself step-level: two steps sharing
// one agent routinely hand it different evidence, and without this the only
// way to give them different ceilings was to duplicate the agent under a
// second name for the sake of one number.
func TestResolveMaxContextBytesPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		step  *int
		agent *int
		want  int
	}{
		{"neither set takes the default", nil, nil, DefaultMaxContextBytes},
		{"the agent's applies when the step is silent", nil, intPtr(5000), 5000},
		{"the step's overrides the agent's", intPtr(250), intPtr(5000), 250},
		{"the step's applies with no agent ceiling", intPtr(250), nil, 250},
		// An explicit 0 is a VALUE — "hand the file over whole" — at either
		// level, and must not read as the absence the default fills in.
		{"the agent's explicit 0 means no ceiling", nil, intPtr(0), 0},
		{"the step's explicit 0 overrides the agent's ceiling", intPtr(0), intPtr(5000), 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if got := resolveMaxContextBytes(test.step, test.agent); got != test.want {
				t.Errorf("resolveMaxContextBytes(%v, %v) = %d, want %d", test.step, test.agent, got, test.want)
			}
		})
	}
}

// TestValidateMaxContextBytesOnSteps covers the step-level rules: a negative
// is meaningless, and a ceiling on a step with no agent governs nothing.
func TestValidateMaxContextBytesOnSteps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		step    Step
		wantErr string
	}{
		{"a negative ceiling is rejected", Step{Agent: "a", MaxContextBytes: intPtr(-1)}, "must not be negative"},
		{"a ceiling on a task step is rejected", Step{Task: "t", Run: "true", MaxContextBytes: intPtr(100)}, "only valid on agent steps"},
		{"an explicit 0 on a task step is still misplaced", Step{Task: "t", Run: "true", MaxContextBytes: intPtr(0)}, "only valid on agent steps"},
		{"unset is fine on any step", Step{Task: "t", Run: "true"}, ""},
		{"set on an agent step is fine", Step{Agent: "a", MaxContextBytes: intPtr(100)}, ""},
		{"0 on an agent step is fine", Step{Agent: "a", MaxContextBytes: intPtr(0)}, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			cfg := &Config{Jobs: []Job{{Name: "j", Plan: []Step{test.step}}}}

			err := cfg.validateMaxContextBytes()
			switch {
			case test.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case test.wantErr != "" && err == nil:
				t.Errorf("expected an error containing %q", test.wantErr)
			case test.wantErr != "" && !strings.Contains(err.Error(), test.wantErr):
				t.Errorf("error = %v, want it to contain %q", err, test.wantErr)
			}
		})
	}
}
