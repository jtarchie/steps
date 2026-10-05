package venue

// The redial an agent must not get.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/shell"
)

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
