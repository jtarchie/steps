package pipeline

import "testing"

// The other half of TestAcrossClassifiesByItsWorstCell, and the one that needs no docker: cells that only FAILED leave the matrix failed, not errored.
func TestAcrossOfFailedCellsIsFailed(t *testing.T) {
	t.Parallel()

	for _, inFlight := range []string{"", "\n    max_in_flight: 2"} {
		collected := runFixturePipeline(t, `
jobs:
- name: build
  plan:
  - across:
    - var: n
      values: ["1", "2"]`+inFlight+`
    task: check-{{ .vars.n }}
    run: "false"
`, true)

		if got := blockFinish(t, collected, "across").Status; got != "failed" {
			t.Errorf("max_in_flight %q: across of two failed cells finished %q, want failed", inFlight, got)
		}
	}
}
