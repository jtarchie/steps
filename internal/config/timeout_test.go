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

// A park that ends by its job's deadline loads; so does one with no timeout of its own, whose default is not counted.
func TestAParkWithinItsJobLoads(t *testing.T) {
	t.Parallel()

	for name, approval := range map[string]string{
		"equal":   "{ message: ship, timeout: 2h }",
		"shorter": "{ message: ship, timeout: 30m }",
		"default": "{ message: ship }",
	} {
		_, err := LoadConfig(writeConfig(t, `
jobs:
- name: publish
  timeout: 2h
  plan:
  - approval: `+approval+`
`))
		if err != nil {
			t.Errorf("%s: LoadConfig: %v", name, err)
		}
	}
}
