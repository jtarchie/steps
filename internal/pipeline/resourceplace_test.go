package pipeline

import (
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// A check has no tree, so a machine reclaimed under it costs nothing to replace: the check is asked again of a fresh one rather than failing the get. Two machines are lost, because a run checks twice and the second check would hide one lost first check.
func TestAReclaimedCheckIsAskedAgainOfAFreshMachine(t *testing.T) {
	ctx, cfg, provider, st, _ := borrowedRun(t, `
resource_types:
- name: probe
  image: alpine:3
  config:
    check: printf '[{"ref":"v1"}]'
    in: "true"

resources:
- name: repo
  type: probe
  tags: [box]
  source: {}

jobs:
- name: build
  plan:
  - get: repo
`)

	var (
		commands atomic.Int32
		placed   atomic.Int32
	)

	previous := newRunner
	newRunner = func(worker venue.Worker, spec shell.RunnerSpec) (shell.Runner, error) {
		if !worker.Here() && placed.Add(1) <= 2 {
			return reclaimedRunner{&commands}, nil
		}

		// The fresh machine is this host, uncontained, so the test needs no daemon.
		worker, spec.Image = venue.Worker{}, ""

		return previous(worker, spec)
	}

	t.Cleanup(func() { newRunner = previous })

	var err error

	_ = captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], st, Run{Provider: provider}) })
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}

	if got := commands.Load(); got != 2 {
		t.Errorf("the reclaimed machines were sent %d commands, want the one check each lost", got)
	}
}
