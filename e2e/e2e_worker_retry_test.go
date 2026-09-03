package e2e

// What a retry means once a step is placed.

import (
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestEndToEndWorkerRetryKeepsScratch is the invariant: attempts: means the
// same thing on a worker as it does here.
//
// A retry is a second go at the SAME workspace. Locally that falls out of the
// directory persisting; a venue session owns a remote scratch and uploads the
// tree when it OPENS, so building the runner inside the retry closure gave
// each attempt a fresh scratch holding the ORIGINAL tree — everything the last
// attempt wrote outside its declared outputs, gone. A task that marks progress
// on disk to skip work it has already done passed here and looped until
// attempts: ran out on a worker.
//
// The marker is deliberately NOT in outputs:. A declared output is fetched
// back and re-sent; the whole point is the undeclared scratch beside it.
func TestEndToEndWorkerRetryKeepsScratch(t *testing.T) {
	for _, placed := range []bool{false, true} {
		name, tags, args := "local", "", []string(nil)
		if placed {
			name, tags, args = "placed", "    tags: [gpu]\n", []string{"--worker", "gpu=local:"}
		}

		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: flaky
`+tags+`    attempts: 2
    outputs: [out]
    run: |
      if [ -f MARKER ]; then
        echo second-attempt > out/f.txt
      else
        touch MARKER
        exit 1
      fi
`)

			// Succeeding at all is the assertion: the second attempt takes
			// the marker branch only if the first attempt's file is still
			// there. A reset scratch touches MARKER again and exits 1.
			err := cli.Run(append([]string{path}, args...))
			if err != nil {
				t.Fatalf("the retried task failed: %v\n\nthe second attempt could not see the marker the first wrote, so its scratch did not survive the retry", err)
			}
		})
	}
}
