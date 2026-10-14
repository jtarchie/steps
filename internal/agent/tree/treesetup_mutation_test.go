package tree

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestReachesTheContainer pins which grants start the step's container: one that would run inside it does, and one with no filesystem of its own never does — that agent starts no container at all.
func TestReachesTheContainer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		specs []config.ToolSpec
		want  bool
	}{
		{"a file tool", []config.ToolSpec{{Builtin: "read_file"}}, true},
		{"run_shell", []config.ToolSpec{{Builtin: "run_shell"}}, true},
		{"a custom tool", []config.ToolSpec{{Name: "lint", Run: "golangci-lint run"}}, true},
		{"web_fetch only", []config.ToolSpec{{Builtin: config.WebFetchBuiltinName}}, false},
		{"nothing", nil, false},
	}

	for _, tc := range cases {
		if got := reachesTheContainer(tc.specs); got != tc.want {
			t.Errorf("%s: reachesTheContainer = %v, want %v", tc.name, got, tc.want)
		}
	}
}
