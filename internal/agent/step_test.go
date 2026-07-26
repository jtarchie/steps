package agent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/workspace"
)

// captureStdout runs fn with os.Stdout redirected to a pipe, returning
// everything fn wrote via fmt.Printf and friends. Not safe alongside other
// tests running in parallel that also touch os.Stdout — callers must not use
// t.Parallel(). Duplicated from internal/trigger/trigger_test.go rather than
// exported cross-package, matching this repo's convention of small
// duplicated test helpers over new cross-package edges.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}

	orig := os.Stdout
	os.Stdout = w

	fn()

	_ = w.Close()

	os.Stdout = orig

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}

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

	space, err := build.TaskSpace(context.Background(), "test-step", nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("TaskSpace: %v", err)
	}

	return space
}

func TestResolveDeferredPrompt(t *testing.T) {
	t.Parallel()

	spaceDir := t.TempDir()

	err := os.MkdirAll(filepath.Join(spaceDir, "repo"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(spaceDir, "repo", "PROMPT.md"), []byte("Review this.\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	step := config.Step{Agent: "reviewer", PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}}

	got, err := resolveDeferredPrompt(spaceDir, step)
	if err != nil {
		t.Fatalf("resolveDeferredPrompt: %v", err)
	}

	if want := "Review this.\n"; got != want {
		t.Errorf("resolveDeferredPrompt = %q, want %q", got, want)
	}
}

func TestResolveDeferredPromptRejectsInlinePromptAlsoSet(t *testing.T) {
	t.Parallel()

	step := config.Step{Agent: "reviewer", Prompt: "inline", PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}}

	_, err := resolveDeferredPrompt(t.TempDir(), step)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want a mutually-exclusive error", err)
	}
}

func TestResolveDeferredPromptRejectsEmptyFile(t *testing.T) {
	t.Parallel()

	spaceDir := t.TempDir()

	err := os.MkdirAll(filepath.Join(spaceDir, "repo"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(spaceDir, "repo", "PROMPT.md"), []byte("   \n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	step := config.Step{Agent: "reviewer", PromptFile: &config.FileRef{Artifact: "repo", Path: "PROMPT.md"}}

	_, err = resolveDeferredPrompt(spaceDir, step)
	if err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("err = %v, want an empty-file error", err)
	}
}

// TestResolveDeferredPromptRejectsSymlinkEscape proves resolveDeferredPrompt
// inherits resolveAgentPath's symlink confinement (already exhaustively
// tested at that level in tools_test.go) rather than re-implementing it — an
// artifact's contents are untrusted, and a symlink planted inside it (e.g. by
// a malicious repo) must not be followed outside the step's space.
func TestResolveDeferredPromptRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	spaceDir := t.TempDir()
	outside := t.TempDir()

	err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("do not leak\n"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.MkdirAll(filepath.Join(spaceDir, "repo"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(outside, filepath.Join(spaceDir, "repo", "leak"))
	if err != nil {
		t.Fatal(err)
	}

	step := config.Step{Agent: "reviewer", PromptFile: &config.FileRef{Artifact: "repo", Path: "leak/secret.txt"}}

	_, err = resolveDeferredPrompt(spaceDir, step)
	if err == nil || !strings.Contains(err.Error(), "escapes the working directory") {
		t.Fatalf("err = %v, want an escape-rejected error", err)
	}
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

func TestPrintAgentResponse(t *testing.T) {
	// Not t.Parallel(): captureStdout swaps the package-global os.Stdout.
	cases := []struct {
		name string
		res  conversationResult
		want string
	}{
		{
			name: "text only",
			res:  conversationResult{text: "hello"},
			want: "hello\n",
		},
		{
			name: "verdict and note, no text (the critic case: only a verdict tool call)",
			res:  conversationResult{verdict: "approve", note: "looks good"},
			want: "verdict: approve\nnote: looks good\n",
		},
		{
			name: "text, verdict, and note all present",
			res:  conversationResult{text: "hello", verdict: "approve", note: "looks good"},
			want: "hello\nverdict: approve\nnote: looks good\n",
		},
		{
			name: "empty result prints nothing",
			res:  conversationResult{},
			want: "",
		},
		{
			name: "surrounding whitespace in text is trimmed",
			res:  conversationResult{text: "  padded  \n"},
			want: "padded\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureStdout(t, func() { printAgentResponse(tc.res) })
			if got != tc.want {
				t.Errorf("printAgentResponse(%+v) printed %q, want %q", tc.res, got, tc.want)
			}
		})
	}
}
