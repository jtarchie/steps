package config

import (
	"testing"
	"time"
)

func TestResolvedAgentTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"unset takes the package default", "", DefaultAgentStepTimeout},
		{"an unparseable value falls back to the default", "twenty minutes", DefaultAgentStepTimeout},
		{"an explicit 0 means no deadline", "0", 0},
		{"a real duration is honored", "5m", 5 * time.Minute},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ResolvedAgentTimeout(tc.in); got != tc.want {
				t.Errorf("ResolvedAgentTimeout(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
