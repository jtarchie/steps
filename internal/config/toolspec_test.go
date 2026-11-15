package config

import "testing"

// TestReachesTree pins which grants start an image: step's container (tree.ForStep): one that runs inside it does, the default grant included, and one with no filesystem of its own never does.
func TestReachesTree(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		specs []ToolSpec
		want  bool
	}{
		{"a file tool", []ToolSpec{{Builtin: ReadFileBuiltinName}}, true},
		{"run_shell", []ToolSpec{{Builtin: RunShellBuiltinName}}, true},
		{"a custom tool", []ToolSpec{{Name: "lint", Run: "golangci-lint run"}}, true},
		{"web_fetch only", []ToolSpec{{Builtin: WebFetchBuiltinName}}, false},
		{"nothing, so the default file tools", nil, true},
	}

	for _, tc := range cases {
		if got := ReachesTree(tc.specs); got != tc.want {
			t.Errorf("%s: ReachesTree = %v, want %v", tc.name, got, tc.want)
		}
	}
}
