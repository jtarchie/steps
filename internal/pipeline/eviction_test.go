package pipeline

// What a spot eviction costs the author: nothing.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/venue"
)

// reclaimedRunner is a placed step's machine taken away on every command, as a drained worker's would be.
type reclaimedRunner struct{ commands *atomic.Int32 }

// evicted wraps the signalled exit the drain killed the command with, as venue does: one that read as the step's own exit would fire on_failure.
func (r reclaimedRunner) evicted() error {
	r.commands.Add(1)

	return fmt.Errorf("%w (EC2 spot terminate): %w", venue.ErrEvicted, &shell.ExitError{Command: "echo never-finishes", Venue: "gpu", Code: 137})
}

func (r reclaimedRunner) Run(context.Context, string) error { return r.evicted() }
func (r reclaimedRunner) RunStreamedCapture(context.Context, string, int) (string, string, error) {
	return "", "", r.evicted()
}
func (r reclaimedRunner) RunCapture(context.Context, string) ([]byte, error) { return nil, r.evicted() }
func (r reclaimedRunner) RunCaptureFull(context.Context, string) (string, string, int, error) {
	return "", "", 0, r.evicted()
}
func (r reclaimedRunner) RunCaptureFullLimited(context.Context, string, int, string) (string, string, int, error) {
	return "", "", 0, r.evicted()
}
func (r reclaimedRunner) RunCaptureFullLimitedStreamed(context.Context, string, int, string) (string, string, int, error) {
	return "", "", 0, r.evicted()
}
func (r reclaimedRunner) WithLabel(string) shell.Runner { return r }
func (reclaimedRunner) Close() error                    { return nil }

// reclaimPlacedSteps makes every placed step's machine a reclaimed one, leaving local steps (hooks) on the host; it counts the commands the reclaimed machines were sent.
func reclaimPlacedSteps(t *testing.T) *atomic.Int32 {
	t.Helper()

	var commands atomic.Int32

	previous := newRunner
	newRunner = func(spec shell.RunnerSpec) (shell.Runner, error) {
		if spec.Worker == "" {
			return previous(spec)
		}

		return reclaimedRunner{&commands}, nil
	}

	t.Cleanup(func() { newRunner = previous })

	return &commands
}

// gpuWorker is static, so an eviction has nowhere to be re-placed and reaches the step as it is.
var gpuWorker = map[string]string{"gpu": "ssh://box"} //nolint:gochecknoglobals // a test fixture

func TestAnEvictionSpendsNoAttempts(t *testing.T) {
	for kind, yaml := range map[string]string{
		"task": `
jobs:
- name: build
  plan:
  - task: doomed
    tags: [gpu]
    attempts: 5
    run: echo never-finishes
`,
		"put": `
resource_types:
- name: probe
  config:
    check: printf '[]'
    out: printf '{"ref":"pushed"}'

resources:
- name: repo
  type: probe
  tags: [gpu]
  source: {}

jobs:
- name: build
  plan:
  - put: repo
    attempts: 5
`,
	} {
		t.Run(kind, func(t *testing.T) {
			commands := reclaimPlacedSteps(t)
			ctx, cfg, provider, st, _ := borrowedRunWith(t, yaml, gpuWorker)

			var err error

			_ = captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) })
			if !errors.Is(err, venue.ErrEvicted) {
				t.Fatalf("RunJob = %v, want the eviction", err)
			}

			if got := commands.Load(); got != 1 {
				t.Errorf("the reclaimed machine was sent %d commands, want 1 — the eviction was billed to the author's attempts:", got)
			}
		})
	}
}

// A reclaimed machine is infrastructure: on_error fires and on_failure, which is for a step that said no, does not.
func TestAnEvictionIsNotAFailure(t *testing.T) {
	reclaimPlacedSteps(t)

	dir := t.TempDir()
	ctx, cfg, provider, st, _ := borrowedRunWith(t, `
jobs:
- name: build
  plan:
  - do:
    - task: doomed
      tags: [gpu]
      run: echo never-finishes
    on_failure:
      task: note
      run: echo failed > `+filepath.Join(dir, "on-failure.txt")+`
    on_error:
      task: note
      run: echo errored > `+filepath.Join(dir, "on-error.txt")+`
`, gpuWorker)

	var err error

	_ = captureStdout(t, func() { err = RunJob(ctx, cfg, &cfg.Jobs[0], nil, provider, st, false) })
	if err == nil {
		t.Fatal("a step on a reclaimed worker reported success")
	}

	_, statErr := os.Stat(filepath.Join(dir, "on-error.txt"))
	if statErr != nil {
		t.Errorf("on_error did not fire: %v", statErr)
	}

	_, statErr = os.Stat(filepath.Join(dir, "on-failure.txt"))
	if statErr == nil {
		t.Error("on_failure fired for a machine the cloud took away")
	}
}
