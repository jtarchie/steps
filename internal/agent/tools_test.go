package agent

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

// testEnv builds a toolEnv over dir using the host runner — the shape every
// tool impl test needs for its third argument.
func testEnv(dir string) toolEnv {
	return toolEnv{dir: dir, runner: shell.HostRunner{}}
}

func TestBuildAgentToolsBuiltins(t *testing.T) {
	t.Parallel()

	t.Run("empty specs enables all built-ins", func(t *testing.T) {
		t.Parallel()

		decls, registry, err := buildAgentTools(nil)
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

		decls, registry, err := buildAgentTools([]config.ToolSpec{{Builtin: "read_file"}})
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

		_, _, err := buildAgentTools([]config.ToolSpec{{Builtin: "nope"}})
		if err == nil {
			t.Error("expected an error for an unknown builtin tool")
		}
	})
}

func TestBuildAgentToolsCustom(t *testing.T) {
	t.Parallel()

	t.Run("custom tool infers params from its run template", func(t *testing.T) {
		t.Parallel()

		decls, registry, err := buildAgentTools([]config.ToolSpec{
			{Name: "post_review", Description: "post a review", Run: `gh pr review {{ .args.action }} -b "{{ .args.body }}"`},
		})
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

		_, _, err := buildAgentTools([]config.ToolSpec{{Builtin: "read_file"}, {Name: "read_file", Run: "echo hi"}})
		if err == nil {
			t.Error("expected an error for a duplicate tool name")
		}
	})

	t.Run("custom tool missing name or run errors", func(t *testing.T) {
		t.Parallel()

		_, _, err := buildAgentTools([]config.ToolSpec{{Description: "no name or run"}})
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

	_, registry, err := buildAgentTools([]config.ToolSpec{
		{Name: "fail_a", Run: "exit 1", Required: true},
		{Name: "fail_b", Run: "exit 1", Required: true},
		{Name: "ok", Run: "true"},
	})
	if err != nil {
		t.Fatal(err)
	}

	calls := []*genai.FunctionCall{
		{ID: "1", Name: "fail_a"},
		{ID: "2", Name: "fail_b"},
		{ID: "3", Name: "ok"},
	}

	parts := toolResponseParts(context.Background(), calls, testEnv(dir), registry)

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
