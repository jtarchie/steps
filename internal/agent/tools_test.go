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

		decls, registry, err := buildAgentTools(nil, nil, "")
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

		decls, registry, err := buildAgentTools(nil, []config.ToolSpec{{Builtin: "read_file"}}, "")
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

		_, _, err := buildAgentTools(nil, []config.ToolSpec{{Builtin: "nope"}}, "")
		if err == nil {
			t.Error("expected an error for an unknown builtin tool")
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

		decls, _, err := buildAgentTools(nil, []config.ToolSpec{{Builtin: "run_shell"}}, image)
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

		decls, registry, err := buildAgentTools(nil, []config.ToolSpec{
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

		_, _, err := buildAgentTools(nil, []config.ToolSpec{{Builtin: "read_file"}, {Name: "read_file", Run: "echo hi"}}, "")
		if err == nil {
			t.Error("expected an error for a duplicate tool name")
		}
	})

	t.Run("custom tool missing name or run errors", func(t *testing.T) {
		t.Parallel()

		_, _, err := buildAgentTools(nil, []config.ToolSpec{{Description: "no name or run"}}, "")
		if err == nil {
			t.Error("expected an error for a custom tool with no name/run")
		}
	})
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

func TestToolResponseParts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, registry, err := buildAgentTools(nil, []config.ToolSpec{
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
