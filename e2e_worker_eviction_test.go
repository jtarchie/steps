package main

// What a spot eviction costs the author: nothing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/wire"
)

// drainingWorkerEnv makes the helper-process shim (see TestMain) impersonate
// a worker being reclaimed: it announces a terminal notice and dies with the
// command signalled. The value is a file that counts how many commands were
// ever attempted against it.
const drainingWorkerEnv = "STEPS_TEST_E2E_DRAINING"

// serveEvictedWorker is the far half of that, hand-rolled the way the venue
// package's variants are: greet, accept the tree, and reclaim the machine on
// the first command.
func serveEvictedWorker(countPath string) {
	decoder := wire.NewDecoder(os.Stdin)
	encoder := wire.NewEncoder(os.Stdout)

	for {
		frame, err := decoder.Read()
		if err != nil {
			os.Exit(1)
		}

		switch frame.Type { //nolint:exhaustive // a stub with opinions about four frames
		case wire.FrameHello:
			_ = writeJSON(encoder, wire.FrameHelloOK, frame.Op, wire.HelloOK{
				Protocol: wire.Protocol, Workdir: os.TempDir(),
			})
		case wire.FrameUpload:
		case wire.FrameEnd:
			_ = encoder.Write(wire.Frame{Type: wire.FrameEnd, Op: frame.Op})
		case wire.FrameExec:
			file, openErr := os.OpenFile(countPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // a path this test's own env names
			if openErr == nil {
				_, _ = file.WriteString("x")
				_ = file.Close()
			}

			_ = writeJSON(encoder, wire.FrameDraining, wire.DrainOp, wire.Draining{
				Reason: "EC2 spot terminate", Terminal: true,
			})
			_ = writeJSON(encoder, wire.FrameExit, frame.Op, wire.Exit{Started: true, Code: -1})

			os.Exit(1)
		}
	}
}

func writeJSON(encoder *wire.Encoder, frameType wire.FrameType, op uint32, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err //nolint:wrapcheck // a test stub
	}

	return encoder.Write(wire.Frame{Type: frameType, Op: op, Payload: data}) //nolint:wrapcheck // a test stub
}

// TestEndToEndEvictionSpendsNoAttempts is the divergence's promise, held
// through the real CLI: attempts: is the author's budget for their own work,
// and the cloud reclaiming a machine spends none of it. Before the retry.Stop
// wrap, this pipeline ground all five attempts against the dead host —
// printing an "(attempt N/5)" line for each, the exact flaky-step reading the
// divergence exists to prevent — and the vacuity audit proved no test
// noticed.
func TestEndToEndEvictionSpendsNoAttempts(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "execs")
	t.Setenv(drainingWorkerEnv, count)

	path := writePipeline(t, dir, `
jobs:
- name: build
  plan:
  - task: doomed
    tags: [gpu]
    attempts: 5
    run: echo never-finishes
`)

	err := run([]string{path, "--worker", "gpu=local:"})
	if err == nil {
		t.Fatal("a step on a permanently reclaimed worker reported success")
	}

	if !strings.Contains(err.Error(), "reclaimed") {
		t.Errorf("error = %v, want it to say the worker was reclaimed", err)
	}

	// One exec per session: the eviction ended the attempts loop rather than
	// spending it. local: is not an acquisition rung, so there is no
	// re-placement either — exactly one command ever ran.
	execs := readFileString(t, count)
	if len(execs) != 1 {
		t.Fatalf("the worker saw %d commands, want 1 — the eviction was billed to the author's attempts:", len(execs))
	}
}
