package config

import (
	"strings"
	"testing"
)

// TestVisionModelsAreOrderedMostSpecificFirst is the same shadowing guard
// contextWindows has: the first matching prefix wins, so an entry whose prefix
// starts with an earlier entry's can never be reached — and the entries that
// matter most here are the `false` exceptions sitting ahead of their family.
func TestVisionModelsAreOrderedMostSpecificFirst(t *testing.T) {
	t.Parallel()

	for i, general := range visionModels {
		for j, specific := range visionModels[i+1:] {
			if strings.HasPrefix(specific.prefix, general.prefix) {
				t.Errorf("visionModels entry %d (%q) is unreachable: entry %d (%q) is a prefix of it and comes first",
					i+1+j, specific.prefix, i, general.prefix)
			}
		}
	}
}

func TestModelSeesImages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		model string
		want  bool
	}{
		{"anthropic/claude-sonnet-4-5", true},
		{"openrouter/anthropic/claude-sonnet-4.5", true},
		{"anthropic/claude-opus-5[1m]", true},
		{"openai/gpt-5.4", true},
		{"openai/gpt-4o-mini", true},
		{"openai/o3", true},
		{"openrouter/google/gemini-2.5-flash", true},
		// The exceptions ahead of their families.
		{"openai/o3-mini", false},
		{"openai/gpt-5.3-codex-spark", false},
		// Matched on the model's own id, not anywhere in the name.
		{"openrouter/moonshotai/kimi-k2-turbo3", false},
		{"openrouter/qwen/qwen3.7-flash", false},
		{"deepseek/deepseek-chat", false},
		{"lmstudio/some-local-build", false},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			t.Parallel()

			got := ModelSeesImages(test.model)
			if got != test.want {
				t.Errorf("ModelSeesImages(%q) = %v, want %v", test.model, got, test.want)
			}
		})
	}
}
