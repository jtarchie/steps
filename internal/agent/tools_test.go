package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// testEnv builds a toolEnv over dir using the host runner — the shape every
// tool impl test needs for its third argument. Goes through NewRunner
// (rather than constructing shell.HostRunner{} directly) so dir is actually
// bound as the runner's cwd — HostRunner's cwd field is unexported, so a
// direct zero-value literal from this package would silently default to an
// empty cwd (the test process's own working directory) instead of dir.
func testEnv(dir string) toolEnv {
	runner, _ := shell.NewRunner("", dir) // host branch never errors
	return toolEnv{dir: dir, runner: runner}
}

func TestBuildAgentToolsBuiltins(t *testing.T) {
	t.Parallel()

	t.Run("empty specs enables all built-ins", func(t *testing.T) {
		t.Parallel()

		decls, registry, _, err := buildAgentTools(context.Background(), nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 3 {
			t.Errorf("got %d declarations, want 3", len(decls.FunctionDeclarations))
		}

		for _, name := range []string{"read_file", "list_dir", "run_shell"} {
			if _, ok := registry[name]; !ok {
				t.Errorf("registry missing %q", name)
			}
		}
	})

	t.Run("selecting a subset omits the rest", func(t *testing.T) {
		t.Parallel()

		decls, registry, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Builtin: "read_file"}}, "")
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 1 {
			t.Errorf("got %d declarations, want 1", len(decls.FunctionDeclarations))
		}

		if _, ok := registry["run_shell"]; ok {
			t.Error("run_shell should not be registered when not selected")
		}
	})

	t.Run("unknown builtin errors", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Builtin: "nope"}}, "")
		if err == nil {
			t.Error("expected an error for an unknown builtin tool")
		}
	})
}

// TestBuildAgentToolsWriteFile is split out from TestBuildAgentToolsBuiltins
// to stay under the linter's per-function complexity budget. write_file is
// deliberately not part of the default grant (see
// config.DefaultAgentToolSpecs): folding a new builtin into "no tools: block
// means every built-in" would change the resolved tool set — and therefore the
// merkle hash — of every existing zero-config agent step. It must be selected
// explicitly, like any other opt-in feature.
func TestBuildAgentToolsWriteFile(t *testing.T) {
	t.Parallel()

	t.Run("not granted by default", func(t *testing.T) {
		t.Parallel()

		_, registry, _, err := buildAgentTools(context.Background(), nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := registry["write_file"]; ok {
			t.Error("write_file should not be granted by default — it must be selected explicitly")
		}
	})

	t.Run("can be selected explicitly", func(t *testing.T) {
		t.Parallel()

		decls, registry, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Builtin: "write_file"}}, "")
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 1 {
			t.Errorf("got %d declarations, want 1", len(decls.FunctionDeclarations))
		}

		if _, ok := registry["write_file"]; !ok {
			t.Error("registry missing write_file")
		}
	})
}

// TestRunShellDescriptionMentionsContainerIsolationOnlyWhenImageSet is split
// out from TestBuildAgentToolsBuiltins to stay under the linter's
// per-function cyclomatic-complexity budget.
func TestRunShellDescriptionMentionsContainerIsolationOnlyWhenImageSet(t *testing.T) {
	t.Parallel()

	runShellDecl := func(t *testing.T, image string) *genai.FunctionDeclaration {
		t.Helper()

		decls, _, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Builtin: "run_shell"}}, image)
		if err != nil {
			t.Fatal(err)
		}

		return decls.FunctionDeclarations[0]
	}

	hostDecl := runShellDecl(t, "")
	if strings.Contains(hostDecl.Description, "fresh, independent container") {
		t.Errorf("host (no image) description shouldn't mention containers: %q", hostDecl.Description)
	}

	containerDecl := runShellDecl(t, "alpine")
	if !strings.Contains(containerDecl.Description, "fresh, independent container") {
		t.Errorf("containerized description should mention per-call container isolation: %q", containerDecl.Description)
	}
}

func TestBuildAgentToolsCustom(t *testing.T) {
	t.Parallel()

	t.Run("custom tool infers params from its run template", func(t *testing.T) {
		t.Parallel()

		decls, registry, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{
			{Name: "post_review", Description: "post a review", Run: `gh pr review {{ .args.action }} -b "{{ .args.body }}"`},
		}, "")
		if err != nil {
			t.Fatal(err)
		}

		if len(decls.FunctionDeclarations) != 1 {
			t.Fatalf("got %d declarations, want 1", len(decls.FunctionDeclarations))
		}

		decl := decls.FunctionDeclarations[0]
		if decl.Name != "post_review" {
			t.Errorf("name = %q, want post_review", decl.Name)
		}

		for _, name := range []string{"action", "body"} {
			if _, ok := decl.Parameters.Properties[name]; !ok {
				t.Errorf("missing inferred param %q", name)
			}
		}

		if len(decl.Parameters.Properties) != 2 {
			t.Errorf("got %d params, want 2", len(decl.Parameters.Properties))
		}

		if _, ok := registry["post_review"]; !ok {
			t.Error("registry missing post_review")
		}
	})

	t.Run("duplicate tool name errors", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Builtin: "read_file"}, {Name: "read_file", Run: "echo hi"}}, "")
		if err == nil {
			t.Error("expected an error for a duplicate tool name")
		}
	})

	t.Run("custom tool missing name or run errors", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{{Description: "no name or run"}}, "")
		if err == nil {
			t.Error("expected an error for a custom tool with no name/run")
		}
	})
}

// TestBuildAgentToolsCustomShellquotePiped guards the security finding that
// agentToolArgPattern only matched a bare {{ .args.NAME }}, missing
// CLAUDE.md's own documented safe idiom for a model-supplied value
// ({{ .args.repo | shellquote }}) — a tool written the recommended way got
// an empty inferred parameter list, so the model's schema never advertised
// the argument, and execCustomTool's missing-argument check (built from the
// same list) never flagged it as missing either. Split into its own
// top-level test (rather than a t.Run under TestBuildAgentToolsCustom) to
// stay under the linter's per-function cognitive-complexity budget.
func TestBuildAgentToolsCustomShellquotePiped(t *testing.T) {
	t.Parallel()

	decls, _, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{
		{Name: "post_review", Description: "post a review", Run: `gh pr review --{{ .args.action }} -b {{ .args.body | shellquote }}`},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	decl := decls.FunctionDeclarations[0]
	for _, name := range []string{"action", "body"} {
		if _, ok := decl.Parameters.Properties[name]; !ok {
			t.Errorf("missing inferred param %q for a piped {{ .args.%s | shellquote }} reference", name, name)
		}
	}

	found := false

	for _, req := range decl.Parameters.Required {
		if req == "body" {
			found = true
		}
	}

	if !found {
		t.Error("body (piped through shellquote) should be required, same as any other inferred param")
	}
}

// TestExecCustomTool confirms a custom tool's failure — required or not —
// is reported as ordinary data, never a Go error; required: is enforced
// entirely by runAgentConversation (see TestRunAgentConversationForces*
// and TestRunAgentConversationRecovers*), not by execCustomTool itself.
func TestExecCustomTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	impl := execCustomTool(config.ToolSpec{Name: "greet", Run: `echo "hello {{ .args.name }}"`}, []string{"name"})

	t.Run("renders args and shells out", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{"name": "world"}, testEnv(dir))
		if result["error"] != nil {
			t.Fatalf("unexpected error: %v", result["error"])
		}

		if stdout, _ := result["stdout"].(string); stdout != "hello world\n" {
			t.Errorf("stdout = %q, want %q", stdout, "hello world\n")
		}
	})

	t.Run("missing required arg yields an error map, not a Go error", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an \"error\" key in the result map")
		}
	})

	t.Run("missing arg is caught before rendering, naming the tool and arg", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{}, testEnv(dir))

		msg, _ := result["error"].(string)
		if want := `greet: missing required argument(s): "name"`; msg != want {
			t.Errorf("error = %q, want %q", msg, want)
		}

		if result["stdout"] != nil || result["exit_code"] != nil {
			t.Errorf("expected no shell execution, got result %v", result)
		}
	})

	t.Run("multiple missing args are named in one error", func(t *testing.T) {
		t.Parallel()

		multiImpl := execCustomTool(config.ToolSpec{Name: "post", Run: `echo {{ .args.action }} {{ .args.body }}`}, []string{"action", "body"})

		result := multiImpl(context.Background(), map[string]any{}, testEnv(dir))

		msg, _ := result["error"].(string)
		if want := `post: missing required argument(s): "action", "body"`; msg != want {
			t.Errorf("error = %q, want %q", msg, want)
		}
	})

	t.Run("empty string arg is treated as missing", func(t *testing.T) {
		t.Parallel()

		result := impl(context.Background(), map[string]any{"name": ""}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an \"error\" key for an empty-string arg")
		}
	})

	t.Run("required: true does not change a nonzero exit's shape", func(t *testing.T) {
		t.Parallel()

		requiredImpl := execCustomTool(config.ToolSpec{Name: "fail", Run: "exit 1", Required: true}, nil)

		result := requiredImpl(context.Background(), map[string]any{}, testEnv(dir))
		if result["exit_code"] != 1 {
			t.Errorf("exit_code = %v, want 1", result["exit_code"])
		}
	})
}

func TestTruncateToolOutput(t *testing.T) {
	t.Parallel()

	t.Run("short output is unchanged", func(t *testing.T) {
		t.Parallel()

		if got := truncateToolOutput("hello"); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("oversized output is capped with a marker", func(t *testing.T) {
		t.Parallel()

		big := strings.Repeat("x", maxToolOutputBytes+500)

		got := truncateToolOutput(big)
		if len(got) <= maxToolOutputBytes {
			t.Errorf("truncated length %d should exceed the cap only by the marker", len(got))
		}

		if !strings.HasPrefix(got, strings.Repeat("x", maxToolOutputBytes)) {
			t.Error("expected the first maxToolOutputBytes to be preserved")
		}

		if !strings.Contains(got, "truncated 500 bytes") {
			t.Errorf("expected a truncation marker, got tail %q", got[len(got)-40:])
		}
	})
}

func TestResolveAgentPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	t.Run("relative path within dir resolves", func(t *testing.T) {
		t.Parallel()

		got, err := resolveAgentPath(dir, "sub/file.txt")
		if err != nil {
			t.Fatal(err)
		}

		if want := filepath.Join(dir, "sub/file.txt"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("absolute path rejected", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAgentPath(dir, "/etc/passwd")
		if err == nil {
			t.Error("expected an error for an absolute path")
		}
	})

	t.Run("traversal outside dir rejected", func(t *testing.T) {
		t.Parallel()

		_, err := resolveAgentPath(dir, "../../etc/passwd")
		if err == nil {
			t.Error("expected an error for a path escaping dir")
		}
	})

	t.Run("nonexistent path is not treated as an escape", func(t *testing.T) {
		t.Parallel()

		got, err := resolveAgentPath(dir, "does/not/exist.txt")
		if err != nil {
			t.Fatalf("unexpected error for a merely-nonexistent path: %v", err)
		}

		if want := filepath.Join(dir, "does/not/exist.txt"); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	// TestResolveAgentPathRejectsSymlinkEscape guards the security finding
	// that the lexical confinement check alone doesn't stop a symlink: a
	// crafted "dir/leak" string satisfies filepath.Clean + HasPrefix even
	// when leak is a symlink pointing anywhere on the host (planted, e.g.,
	// via run_shell, which has no path confinement of its own).
	t.Run("symlink escaping dir is rejected", func(t *testing.T) {
		t.Parallel()

		symlinkDir := t.TempDir()
		outside := t.TempDir()

		secret := filepath.Join(outside, "secret.txt")

		err := os.WriteFile(secret, []byte("should not leak"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		err = os.Symlink(outside, filepath.Join(symlinkDir, "leak"))
		if err != nil {
			t.Fatal(err)
		}

		_, err = resolveAgentPath(symlinkDir, "leak/secret.txt")
		if err == nil {
			t.Error("expected an error for a path resolving outside dir via a symlink")
		}
	})
}

func TestShellToolResult(t *testing.T) {
	t.Parallel()

	t.Run("output under the cap is returned untouched", func(t *testing.T) {
		t.Parallel()

		result := shellToolResult(context.Background(), "echo hi", testEnv(t.TempDir()))
		if result["stdout"] != "hi\n" {
			t.Errorf("stdout = %v, want %q", result["stdout"], "hi\n")
		}
	})

	t.Run("output over maxToolOutputBytes is capped, not just truncated after full buffering", func(t *testing.T) {
		t.Parallel()

		command := "yes x | head -c " + strconv.Itoa(maxToolOutputBytes+500)
		result := shellToolResult(context.Background(), command, testEnv(t.TempDir()))

		stdout, ok := result["stdout"].(string)
		if !ok {
			t.Fatalf("stdout = %v (%T), want a string", result["stdout"], result["stdout"])
		}

		if !strings.Contains(stdout, "truncated 500 bytes") {
			t.Errorf("stdout does not contain the expected truncation marker; got suffix %q", stdout[max(0, len(stdout)-60):])
		}

		body := strings.SplitN(stdout, "\n... [truncated", 2)[0]
		if len(body) != maxToolOutputBytes {
			t.Errorf("retained body length = %d, want %d", len(body), maxToolOutputBytes)
		}
	})
}

// TestShellToolResultSpillDir is split out from TestShellToolResult to keep
// that function's cyclomatic complexity under the linter's cap.
func TestShellToolResultSpillDir(t *testing.T) {
	t.Parallel()

	spillDir := t.TempDir()
	env := testEnv(t.TempDir())
	env.spillDir = spillDir

	command := "yes x | head -c " + strconv.Itoa(maxToolOutputBytes+500)
	result := shellToolResult(context.Background(), command, env)

	stdout, ok := result["stdout"].(string)
	if !ok {
		t.Fatalf("stdout = %v (%T), want a string", result["stdout"], result["stdout"])
	}

	if !strings.Contains(stdout, "output too large") {
		t.Errorf("stdout does not mention \"output too large\"; got %q", stdout)
	}

	entries, err := os.ReadDir(spillDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%q) = (%v entries, %v), want exactly one spill file", spillDir, entries, err)
	}

	spillPath := filepath.Join(spillDir, entries[0].Name())
	if !strings.Contains(stdout, spillPath) {
		t.Errorf("stdout does not name the spill file path %q; got %q", spillPath, stdout)
	}

	data, err := os.ReadFile(spillPath) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", spillPath, err)
	}

	if len(data) != maxToolOutputBytes+500 {
		t.Errorf("spill file length = %d, want the full %d bytes", len(data), maxToolOutputBytes+500)
	}
}

func TestExecReadFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("read_file returns content", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "a.txt"}, testEnv(dir))
		if result["content"] != "hello" {
			t.Errorf("content = %v, want %q", result["content"], "hello")
		}
	})

	t.Run("read_file rejects traversal", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "../../etc/passwd"}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for a traversal path")
		}
	})

	t.Run("read_file requires path", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for a missing path argument")
		}
	})

	t.Run("a file over maxToolOutputBytes is truncated with an accurate marker", func(t *testing.T) {
		t.Parallel()

		bigDir := t.TempDir()
		extra := 1234
		big := strings.Repeat("x", maxToolOutputBytes+extra)

		err := os.WriteFile(filepath.Join(bigDir, "big.txt"), []byte(big), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		result := execReadFile(context.Background(), map[string]any{"path": "big.txt"}, testEnv(bigDir))

		content, ok := result["content"].(string)
		if !ok {
			t.Fatalf("content = %v (%T), want a string", result["content"], result["content"])
		}

		wantMarker := fmt.Sprintf("\n... [truncated %d bytes]", extra)
		if !strings.HasSuffix(content, wantMarker) {
			t.Errorf("content does not end with the expected marker %q; got suffix %q", wantMarker, content[max(0, len(content)-60):])
		}

		gotBody := strings.TrimSuffix(content, wantMarker)
		if len(gotBody) != maxToolOutputBytes {
			t.Errorf("truncated body length = %d, want %d", len(gotBody), maxToolOutputBytes)
		}
	})
}

// writeLines writes n newline-joined "line N" records to path.
func writeLines(t *testing.T, path string, n int) {
	t.Helper()

	lines := make([]string, n)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}

	err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600)
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecReadFileLineRange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "lines.txt"), 10)

	result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "start_line": float64(3), "end_line": float64(5)}, testEnv(dir))

	if result["content"] != "line 3\nline 4\nline 5" {
		t.Errorf("content = %v, want %q", result["content"], "line 3\nline 4\nline 5")
	}

	if result["start_line"] != 3 {
		t.Errorf("start_line = %v, want 3", result["start_line"])
	}

	if result["end_line"] != 5 {
		t.Errorf("end_line = %v, want 5", result["end_line"])
	}

	if result["truncated"] != false {
		t.Errorf("truncated = %v, want false", result["truncated"])
	}
}

func TestExecReadFileLineRangeOnlyStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "lines.txt"), 5)

	result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "start_line": float64(4)}, testEnv(dir))

	if result["content"] != "line 4\nline 5" {
		t.Errorf("content = %v, want %q (start_line with no end_line reads to EOF)", result["content"], "line 4\nline 5")
	}

	if result["end_line"] != 5 {
		t.Errorf("end_line = %v, want 5", result["end_line"])
	}
}

func TestExecReadFileLineRangeOnlyEnd(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "lines.txt"), 5)

	result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "end_line": float64(2)}, testEnv(dir))

	if result["content"] != "line 1\nline 2" {
		t.Errorf("content = %v, want %q (end_line with no start_line defaults start_line to 1)", result["content"], "line 1\nline 2")
	}

	if result["start_line"] != 1 {
		t.Errorf("start_line = %v, want 1", result["start_line"])
	}
}

func TestExecReadFileLineRangeBeyondEOF(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "lines.txt"), 3)

	result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "start_line": float64(100)}, testEnv(dir))

	if result["content"] != "" {
		t.Errorf("content = %v, want \"\" (start_line beyond EOF)", result["content"])
	}

	if result["end_line"] != 99 {
		t.Errorf("end_line = %v, want 99 (start_line - 1, signaling no lines matched)", result["end_line"])
	}
}

func TestExecReadFileLineRangeInvalid(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLines(t, filepath.Join(dir, "lines.txt"), 5)

	t.Run("start_line < 1 is an error", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "start_line": float64(0)}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for start_line < 1")
		}
	})

	t.Run("end_line < start_line is an error", func(t *testing.T) {
		t.Parallel()

		result := execReadFile(context.Background(), map[string]any{"path": "lines.txt", "start_line": float64(3), "end_line": float64(2)}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for end_line < start_line")
		}
	})
}

func TestExecReadFileLineRangeTruncatesAtByteCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// One line per byte budget slot: 3 lines of ~maxToolOutputBytes/2 each
	// guarantees the second line alone doesn't fit alongside the first,
	// forcing the cap to trip mid-range rather than at EOF.
	longLine := strings.Repeat("x", maxToolOutputBytes/2)
	content := strings.Join([]string{longLine, longLine, longLine}, "\n")

	err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(content), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result := execReadFile(context.Background(), map[string]any{"path": "big.txt", "start_line": float64(1)}, testEnv(dir))

	if result["truncated"] != true {
		t.Errorf("truncated = %v, want true", result["truncated"])
	}

	endLine, ok := result["end_line"].(int)
	if !ok || endLine >= 3 {
		t.Errorf("end_line = %v, want < 3 (the byte cap should stop the range before EOF)", result["end_line"])
	}
}

// TestExecReadFileLineRangeSingleLongLine covers the shape spilled command
// output most often takes — a single line longer than the return budget with
// no newlines (base64, minified JSON, `jq -c`). A plain bufio.Scanner would
// return bufio.ErrTooLong and surface a cryptic error; read_file must instead
// hand back a byte-truncated prefix with truncated=true, matching the capped
// prefix a no-range read gives, so the documented spill-recovery path works.
func TestExecReadFileLineRangeSingleLongLine(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// One line, no trailing newline, larger than the return budget but under
	// the scan buffer bound — the common spilled-output shape.
	oneLongLine := strings.Repeat("y", maxToolOutputBytes+5000)

	err := os.WriteFile(filepath.Join(dir, "blob.txt"), []byte(oneLongLine), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result := execReadFile(context.Background(), map[string]any{"path": "blob.txt", "start_line": float64(1)}, testEnv(dir))

	if result["error"] != nil {
		t.Fatalf("read_file returned an error for a long single line: %v", result["error"])
	}

	content, ok := result["content"].(string)
	if !ok {
		t.Fatalf("content = %v (%T), want a string", result["content"], result["content"])
	}

	if len(content) != maxToolOutputBytes {
		t.Errorf("content length = %d, want a %d-byte prefix of the long line", len(content), maxToolOutputBytes)
	}

	if result["truncated"] != true {
		t.Errorf("truncated = %v, want true", result["truncated"])
	}
}

// TestExecReadFileLineRangeLineOverScanBound covers a line so large it exceeds
// even the scan buffer bound: rather than a hard bufio.ErrTooLong, read_file
// degrades to truncated=true (with readFileFull still available as a fallback).
func TestExecReadFileLineRangeLineOverScanBound(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	huge := strings.Repeat("z", maxReadFileScanBytes+1)

	err := os.WriteFile(filepath.Join(dir, "huge.txt"), []byte(huge), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	result := execReadFile(context.Background(), map[string]any{"path": "huge.txt", "start_line": float64(1)}, testEnv(dir))

	if result["error"] != nil {
		t.Fatalf("read_file returned a hard error for a line over the scan bound: %v", result["error"])
	}

	if result["truncated"] != true {
		t.Errorf("truncated = %v, want true", result["truncated"])
	}
}

// TestExecReadFileCanReadSpilledRunShellOutput is an end-to-end check of the
// fix this feature exists for: read_file was confined to the step's working
// directory (resolveAgentPath), but a run_shell/custom tool's spilled output
// used to live in a top-level os.MkdirTemp dir outside it — reachable only
// via run_shell itself. newToolOutputSpillDir now nests the spill directory
// under the working directory instead, so the same path run_shell's pointer
// message names (once made relative to the working directory, which the
// model already knows from its system message — see buildSystemMessage) is
// one read_file can open.
func TestExecReadFileCanReadSpilledRunShellOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	env := testEnv(dir)
	env.spillDir = newToolOutputSpillDir(dir, "test-agent")

	if env.spillDir == "" {
		t.Fatal("newToolOutputSpillDir returned \"\"")
	}

	command := "yes x | head -c " + strconv.Itoa(maxToolOutputBytes+500)
	shellResult := shellToolResult(context.Background(), command, env)

	stdout, ok := shellResult["stdout"].(string)
	if !ok {
		t.Fatalf("stdout = %v (%T), want a string", shellResult["stdout"], shellResult["stdout"])
	}

	absPath := extractSpillPath(t, stdout)

	relPath, err := filepath.Rel(dir, absPath)
	if err != nil || strings.HasPrefix(relPath, "..") {
		t.Fatalf("spill path %q is not inside the working directory %q (rel = %q, err = %v)", absPath, dir, relPath, err)
	}

	readResult := execReadFile(context.Background(), map[string]any{"path": relPath, "start_line": float64(1), "end_line": float64(1)}, env)
	if readResult["error"] != nil {
		t.Fatalf("read_file(%q) = error: %v, want the spilled file to be readable", relPath, readResult["error"])
	}

	if readResult["content"] != "x" {
		t.Errorf("content = %v, want %q", readResult["content"], "x")
	}
}

// extractSpillPath pulls the absolute path out of a spillWriter pointer
// message ("output too large (...); full output saved to <path>\n\n...").
func extractSpillPath(t *testing.T, message string) string {
	t.Helper()

	const marker = "full output saved to "

	idx := strings.Index(message, marker)
	if idx < 0 {
		t.Fatalf("message does not contain %q; got %q", marker, message)
	}

	rest := message[idx+len(marker):]

	end := strings.IndexByte(rest, '\n')
	if end < 0 {
		end = len(rest)
	}

	return rest[:end]
}

// TestExecReadFileRejectsSymlinkEscape reproduces the security finding's
// exact scenario: a model calls run_shell("ln -s /some/secret leak")
// (run_shell has no path confinement at all), then read_file("leak"). The
// lexical check in resolveAgentPath alone would pass ("dir/leak" stays
// inside dir as a string), but os.ReadFile dereferences the symlink at the
// OS level — so this must be caught before execReadFile ever calls
// os.Open. Split into its own top-level test (rather than a t.Run under
// TestExecReadFile) to stay under the linter's per-function cyclomatic-
// complexity budget, matching this file's existing convention for that.
func TestExecReadFileRejectsSymlinkEscape(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "passwd")

	err := os.WriteFile(secret, []byte("root:x:0:0"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.Symlink(secret, filepath.Join(dir, "leak"))
	if err != nil {
		t.Fatal(err)
	}

	result := execReadFile(context.Background(), map[string]any{"path": "leak"}, testEnv(dir))
	if result["error"] == nil {
		t.Errorf("expected an error for read_file(\"leak\") through a symlink escaping dir, got content %v", result["content"])
	}
}

func TestExecListDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("list_dir defaults to the working directory", func(t *testing.T) {
		t.Parallel()

		result := execListDir(context.Background(), map[string]any{}, testEnv(dir))

		entries, ok := result["entries"].([]map[string]any)
		if !ok || len(entries) != 1 {
			t.Fatalf("entries = %v", result["entries"])
		}
	})
}

// mustExecWriteFileOK runs write_file against dir and fails the test if it
// reports an error — the shared success assertion every TestExecWriteFile
// case needs, factored out so the table of cases below stays a flat list of
// branches instead of a repeated if-err block per case (which is what was
// tripping the linter's per-function complexity budget).
func mustExecWriteFileOK(t *testing.T, dir string, args map[string]any) {
	t.Helper()

	result := execWriteFile(context.Background(), args, testEnv(dir))
	if result["error"] != nil {
		t.Fatalf("unexpected error: %v", result["error"])
	}
}

// readTestFile reads back a.txt under dir — every case here writes to the
// same fixed name, and dir is always t.TempDir()-owned, so this is never
// attacker-influenced despite the dynamic path.
func readTestFile(t *testing.T, dir string) string {
	t.Helper()

	got, err := os.ReadFile(filepath.Join(dir, "a.txt")) //nolint:gosec // test-owned path under t.TempDir()
	if err != nil {
		t.Fatal(err)
	}

	return string(got)
}

func TestExecWriteFile(t *testing.T) {
	t.Parallel()

	t.Run("writes a new file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		mustExecWriteFileOK(t, dir, map[string]any{"path": "a.txt", "content": "hello"})

		if got := readTestFile(t, dir); got != "hello" {
			t.Errorf("got %q, want %q", got, "hello")
		}
	})

	t.Run("overwrites an existing file by default", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		mustExecWriteFileOK(t, dir, map[string]any{"path": "a.txt", "content": "new"})

		if got := readTestFile(t, dir); got != "new" {
			t.Errorf("got %q, want %q", got, "new")
		}
	})

	t.Run("append: true appends instead of overwriting", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		mustExecWriteFileOK(t, dir, map[string]any{"path": "a.txt", "content": "new", "append": true})

		if got := readTestFile(t, dir); got != "oldnew" {
			t.Errorf("got %q, want %q", got, "oldnew")
		}
	})

	t.Run("empty content is a valid write, not a missing-argument error", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		mustExecWriteFileOK(t, dir, map[string]any{"path": "a.txt", "content": ""})

		if got := readTestFile(t, dir); got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

// TestExecWriteFileRejectsBadPaths covers write_file's argument validation
// and path-confinement errors — split from TestExecWriteFile (which covers
// the successful-write shapes) to stay under the linter's per-function
// complexity budget.
func TestExecWriteFileRejectsBadPaths(t *testing.T) {
	t.Parallel()

	t.Run("missing path is an error", func(t *testing.T) {
		t.Parallel()

		result := execWriteFile(context.Background(), map[string]any{"content": "hello"}, testEnv(t.TempDir()))
		if result["error"] == nil {
			t.Error("expected an error for a missing path")
		}
	})

	t.Run("missing content is an error", func(t *testing.T) {
		t.Parallel()

		result := execWriteFile(context.Background(), map[string]any{"path": "a.txt"}, testEnv(t.TempDir()))
		if result["error"] == nil {
			t.Error("expected an error for missing content")
		}
	})

	t.Run("rejects traversal outside dir", func(t *testing.T) {
		t.Parallel()

		result := execWriteFile(context.Background(), map[string]any{"path": "../../etc/passwd", "content": "x"}, testEnv(t.TempDir()))
		if result["error"] == nil {
			t.Error("expected an error for a path escaping dir")
		}
	})

	t.Run("missing parent directory is an error, not auto-created", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		result := execWriteFile(context.Background(), map[string]any{"path": "sub/a.txt", "content": "hello"}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for a nonexistent parent directory")
		}
	})

	// This case guards the gap resolveAgentPath alone leaves open for a
	// brand-new file: EvalSymlinks fails with ENOENT on a nonexistent leaf
	// regardless of whether an ancestor directory is a symlink, so without
	// resolveWritePath's extra parent-directory check, a write through a
	// symlinked parent (planted, e.g., via run_shell) would silently write
	// outside dir.
	t.Run("rejects a write through a symlinked parent directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		outside := t.TempDir()

		err := os.Symlink(outside, filepath.Join(dir, "leak"))
		if err != nil {
			t.Fatal(err)
		}

		result := execWriteFile(context.Background(), map[string]any{"path": "leak/newfile.txt", "content": "x"}, testEnv(dir))
		if result["error"] == nil {
			t.Error("expected an error for a write through a symlinked parent directory escaping dir")
		}

		_, statErr := os.Stat(filepath.Join(outside, "newfile.txt"))
		if statErr == nil {
			t.Error("write escaped dir via the symlinked parent")
		}
	})
}

func TestToolResponseParts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, registry, _, err := buildAgentTools(context.Background(), nil, []config.ToolSpec{
		{Name: "fail_a", Run: "exit 1", Required: true},
		{Name: "fail_b", Run: "exit 1", Required: true},
		{Name: "ok", Run: "true"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	calls := []*genai.FunctionCall{
		{ID: "1", Name: "fail_a"},
		{ID: "2", Name: "fail_b"},
		{ID: "3", Name: "ok"},
	}

	parts := toolResponseParts(context.Background(), calls, testEnv(dir), registry, nil, map[string]int{})

	if len(parts) != 3 {
		t.Fatalf("expected a response part for every call, even after a failed one, got %d", len(parts))
	}

	got := make(map[string]int, 3)
	for _, part := range parts {
		code, _ := part.FunctionResponse.Response["exit_code"].(int)
		got[part.FunctionResponse.Name] = code
	}

	want := map[string]int{"fail_a": 1, "fail_b": 1, "ok": 0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("exit codes = %v, want %v", got, want)
	}
}
