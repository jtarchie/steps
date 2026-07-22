package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/internal/workspace"
)

func TestNewToolOutputSpillDir(t *testing.T) {
	t.Parallel()

	workDir := t.TempDir()

	spillDir := newToolOutputSpillDir(workDir, "test-agent")
	if spillDir == "" {
		t.Fatal("newToolOutputSpillDir returned \"\", want a directory path")
	}

	if want := filepath.Join(workDir, toolOutputSpillDirName); spillDir != want {
		t.Errorf("newToolOutputSpillDir = %q, want %q (a subdirectory of the working directory, so read_file/list_dir can reach it)", spillDir, want)
	}

	info, err := os.Stat(spillDir)
	if err != nil {
		t.Fatalf("Stat(%q): %v", spillDir, err)
	}

	if !info.IsDir() {
		t.Errorf("newToolOutputSpillDir returned %q, want a directory", spillDir)
	}

	// resolveAgentPath is exactly what read_file/list_dir use to confine a
	// model-supplied relative path to the working directory — the spilled
	// output must actually be reachable that way, not just physically nested.
	resolved, err := resolveAgentPath(workDir, toolOutputSpillDirName)
	if err != nil {
		t.Errorf("resolveAgentPath(%q, %q) = error: %v, want the spill dir to be reachable via read_file/list_dir", workDir, toolOutputSpillDirName, err)
	} else if resolved != spillDir {
		t.Errorf("resolveAgentPath(%q, %q) = %q, want %q", workDir, toolOutputSpillDirName, resolved, spillDir)
	}
}

// testStepSpace builds a real StepSpace (the default, non-isolated
// implementation) so preparedAgentStep.close's workspace.CloseSpace call has
// a concrete, non-nil StepSpace to invoke — a zero-value preparedAgentStep's
// nil StepSpace interface would panic on .Close().
func testStepSpace(t *testing.T) workspace.StepSpace {
	t.Helper()

	provider, err := workspace.NewProvider(nil)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	build, err := provider.NewBuild(context.Background(), "test-build")
	if err != nil {
		t.Fatalf("NewBuild: %v", err)
	}
	t.Cleanup(func() { workspace.CloseBuild(build, "test-build") })

	space, err := build.TaskSpace(context.Background(), "test-step", nil, nil)
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	return space
}

func TestPreparedAgentStepCloseRemovesSpillDir(t *testing.T) {
	t.Parallel()

	spillDir := newToolOutputSpillDir(t.TempDir(), "test-agent")
	if spillDir == "" {
		t.Fatal("newToolOutputSpillDir returned \"\"")
	}

	prepared := preparedAgentStep{space: testStepSpace(t), spillDir: spillDir}
	prepared.close("test-agent")

	_, err := os.Stat(spillDir)
	if !os.IsNotExist(err) {
		t.Errorf("spill dir %q still exists after close(): stat err = %v, want IsNotExist", spillDir, err)
	}
}

func TestPreparedAgentStepCloseToleratesEmptySpillDir(t *testing.T) {
	t.Parallel()

	prepared := preparedAgentStep{space: testStepSpace(t)}
	prepared.close("test-agent") // must not panic on a zero-value spillDir
}
