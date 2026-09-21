package venue

// The redial an agent must not get.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

// TestVenueNoRedialFailsRatherThanRewindingTheTree is the half of the redial contract a conversation needs and a task does not: a task's command re-runs from the top, so re-sending the step's inputs is exactly right, while an agent has been editing for several turns and only what it wrote under outputs: has come home — so a re-sent tree is those edits silently undone, and the next tool call would succeed against a directory rewound underneath a model that was never told.
func TestVenueNoRedialFailsRatherThanRewindingTheTree(t *testing.T) {
	t.Parallel()

	cwd := filepath.Join(t.TempDir(), "noredial-after-kill")
	mustMkdir(t, filepath.Join(cwd, "out"))

	spec := localWorker(t, cwd, "out")
	spec.NoRedial = true

	runner := newLocalRunner(t, spec)

	err := runner.Run(context.Background(), "echo one > out/first.txt")
	if err != nil {
		t.Fatalf("the first command failed: %v", err)
	}

	err = killTheShim(t, runner)
	if !errors.Is(err, errWorkerLost) || shell.IsExitError(err) {
		t.Fatalf("the command whose worker died returned %v, want errWorkerLost and not a command's exit", err)
	}

	// The next command must NOT reach a fresh shim. Under the ordinary contract this succeeds against a re-uploaded tree, which is the outcome this flag exists to prevent.
	err = runner.Run(context.Background(), "true")
	if err == nil {
		t.Fatal("the command after the worker died succeeded; the session redialled and re-sent the tree under a conversation that had moved on")
	}

	if !errors.Is(err, errSessionGone) {
		t.Errorf("error = %v, want errSessionGone so the step fails for a reason its author can read", err)
	}
}

// TestVenueReadOnlySkipsTheFetch covers what a conversation's dozens of reads would otherwise cost: the fetch after every command is right for a task, where one command is followed by assert: reading the local tree, and wrong for an agent issuing a read_file per turn, each paying a full transfer of outputs nothing touched. The marker is written locally AFTER the command, so a fetch that ran would overwrite it with the worker's copy.
func TestVenueReadOnlySkipsTheFetch(t *testing.T) {
	t.Parallel()

	cwd := filepath.Join(t.TempDir(), "readonly-fetch")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, localWorker(t, cwd, "out"))

	err := runner.Run(context.Background(), "echo worker > out/who.txt")
	if err != nil {
		t.Fatalf("seeding the worker's copy failed: %v", err)
	}

	local := filepath.Join(cwd, "out", "who.txt")
	mustWrite(t, local, "local\n")

	// A read-only command: the worker's own copy still says "worker", so a fetch would replace what was just written here.
	_, _, _, err = runner.RunCaptureFullLimited(shell.ReadOnly(context.Background()), "cat out/who.txt", 4096, "")
	if err != nil {
		t.Fatalf("the read-only command failed: %v", err)
	}

	if got := mustRead(t, local); got != "local\n" {
		t.Errorf("out/who.txt = %q, want %q — a command marked read-only still fetched the worker's outputs", got, "local\n")
	}

	// And an unmarked command still fetches, or the marker above would prove nothing about the marking.
	err = runner.Run(context.Background(), "true")
	if err != nil {
		t.Fatalf("the ordinary command failed: %v", err)
	}

	if got := mustRead(t, local); got != "worker\n" {
		t.Errorf("out/who.txt = %q, want the worker's copy back — an unmarked command must still fetch", got)
	}
}
