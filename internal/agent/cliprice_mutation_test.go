package agent

import (
	"strings"
	"testing"
)

// Not t.Parallel(): captureLogs swaps the default logger.
func TestAttemptCostWarnsOnlyWhenSpendGoesUnpriced(t *testing.T) {
	cases := []struct {
		name  string
		model string
		usage cliUsage
		warn  bool
	}{
		{"unknown model, output only", "mystery-model", cliUsage{OutputTokens: 5}, true},
		{"unknown model, nothing spent", "mystery-model", cliUsage{}, false},
		{"known model", "sonnet", cliUsage{InputTokens: 50_000}, false},
	}

	for _, tc := range cases {
		logs := captureLogs(t)

		attemptCost(tc.model, cliRunResult{streamed: tc.usage})

		if got := strings.Contains(logs.String(), "agent.cli.unpriced_attempt"); got != tc.warn {
			t.Errorf("%s: warned = %v, want %v: %s", tc.name, got, tc.warn, logs.String())
		}
	}
}
