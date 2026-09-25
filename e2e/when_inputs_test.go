package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestWhenInputsRefusedByValidate carries each when.inputs refusal through
// fileProblems — the same path the daemon's accept takes, so a `pipeline set`
// gets these back over HTTP.
func TestWhenInputsRefusedByValidate(t *testing.T) {
	cases := []struct {
		name    string
		plan    string
		wantErr string
	}{
		{
			name: "an input nothing produces",
			plan: `  - task: react
    when:
      inputs: [answer]
      run: test -s answer/reply.md
    run: echo reacted`,
			wantErr: `when input "answer" is not a resource fetched or an output produced earlier in the plan`,
		},
		{
			name: "an input the step already declares",
			plan: `  - task: draft
    outputs: [answer]
    run: echo hi > answer/reply.md
  - task: react
    inputs: [answer]
    when:
      inputs: [answer]
      run: test -s answer/reply.md
    run: echo reacted`,
			wantErr: "already an input of the step",
		},
		{
			name: "inputs: all",
			plan: `  - task: react
    when:
      inputs: all
      run: "true"
    run: echo reacted`,
			wantErr: "inputs must be a list of artifact names",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writePipeline(t, t.TempDir(), "jobs:\n- name: j\n  plan:\n"+tc.plan+"\n")

			err := cli.Run([]string{"validate", path})
			if err == nil {
				t.Fatal("the pipeline validated")
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
