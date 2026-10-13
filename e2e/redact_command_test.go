package e2e

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// A rendered command routinely carries a source: credential in a URL; a failing one must not copy it into the error a terminal prints and the run's events keep.
func TestAFailingCommandDoesNotLeakItsURLCredentials(t *testing.T) {
	path := writePipeline(t, t.TempDir(), `
jobs:
- name: review
  plan:
  - task: fetch
    inputs: []
    run: "true https://u:hunter2secret@example.com/repo.git; exit 3"
`)

	err := cli.Run([]string{"run", path, "--job", "review"})
	if err == nil {
		t.Fatal("the task was supposed to fail")
	}

	if strings.Contains(err.Error(), "hunter2secret") {
		t.Errorf("the run's error leaks the credential: %v", err)
	}

	if !strings.Contains(err.Error(), "u:***@example.com") {
		t.Errorf("the run's error = %v, want the command with its password masked", err)
	}

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "review", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}

	for _, row := range rows {
		for _, field := range []string{row.Text, row.Name, row.Detail} {
			if strings.Contains(field, "hunter2secret") {
				t.Errorf("%s event leaks the credential: %q", row.Type, field)
			}
		}
	}
}
