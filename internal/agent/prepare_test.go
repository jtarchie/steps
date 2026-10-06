package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

type dirSpace string

func (d dirSpace) Dir() string                 { return string(d) }
func (dirSpace) Capture(context.Context) error { return nil }
func (dirSpace) Close() error                  { return nil }

// TestSpillDirGoesBeforeCaptureOnlyWhenCaptureWouldTakeIt: a spill dir inside a declared output would ride into the artifact, so it goes before Capture; anywhere else it stays, because --keep-workspace keeps it as the evidence the transcript's pointer messages name.
func TestSpillDirGoesBeforeCaptureOnlyWhenCaptureWouldTakeIt(t *testing.T) {
	t.Parallel()

	for name, spillUnder := range map[string]string{"inside an output": "built", "beside the outputs": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			spillDir := newToolOutputSpillDir(filepath.Join(dir, spillUnder), "test-agent")

			prepared := preparedAgentStep{step: config.Step{Outputs: []string{"built"}}, space: dirSpace(dir), spillDir: spillDir}
			prepared.removeSpillDirIfCaptured()

			_, err := os.Stat(spillDir)
			if removed := os.IsNotExist(err); removed != (spillUnder != "") {
				t.Errorf("spill dir %s removed = %v, want it removed only when an output holds it", spillDir, removed)
			}
		})
	}
}

// TestPrepareStepTreeHandsBackTheTreeAndItsContext is the fix agent's preparation: a host tree over its directory, and the context_paths files read with read_file granted — and refused without it, since nothing could read them back.
func TestPrepareStepTreeHandsBackTheTreeAndItsContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("the build is flaky on Tuesdays\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	ri := config.ResolvedInvocation{ContextPaths: []string{"notes.md"}}
	granted := &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "run_shell"}, {Name: "read_file"}}}

	files, modelDir, blocks, err := prepareStepTree(t.Context(), nil, ri, dir, granted, &lostTree{})
	if err != nil {
		t.Fatalf("prepareStepTree: %v", err)
	}

	if files == nil || modelDir != dir || len(blocks) != 1 {
		t.Errorf("prepareStepTree = %v, %q, %d block(s); want a tree over %q and the one context file", files, modelDir, len(blocks), dir)
	}

	withoutReadFile := &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "run_shell"}}}

	_, _, _, err = prepareStepTree(t.Context(), nil, ri, dir, withoutReadFile, &lostTree{})
	if err == nil {
		t.Error("context_paths without read_file was accepted; the model could never read those files back")
	}
}
