package toolreg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileReadConfinement(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := New()
	out, err := r.Call(context.Background(), "file.read", map[string]any{"root": root, "path": "ok.txt"})
	if err != nil || out["content"] != "inside" {
		t.Fatalf("confined read failed: %v %v", out, err)
	}
	// Model-authored escapes must be refused, not resolved.
	for _, escape := range []string{"../secret.txt", "../../etc/passwd", "/etc/passwd"} {
		if _, err := r.Call(context.Background(), "file.read", map[string]any{"root": root, "path": escape}); err == nil {
			t.Errorf("path %q should be refused", escape)
		}
	}
}

func TestDiffSplitEnrichment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(strings.Repeat("x", 100)), 0o644); err != nil {
		t.Fatal(err)
	}
	diff := "diff --git a/a.go b/a.go\n+new line\ndiff --git a/gone.go b/gone.go\n-old line\n"

	r := New()
	out, err := r.Call(context.Background(), "diff.split", map[string]any{
		"diff": diff, "root": root, "context_bytes": "40",
	})
	if err != nil {
		t.Fatal(err)
	}
	files := out["files"].([]any)
	a := files[0].(map[string]any)
	content, _ := a["content"].(string)
	if !strings.HasPrefix(content, "xxxx") || !strings.Contains(content, "truncated") {
		t.Errorf("a.go content = %q, want truncated head", content)
	}
	// Deleted files simply carry no content.
	gone := files[1].(map[string]any)
	if _, has := gone["content"]; has {
		t.Errorf("gone.go should have no content, got %v", gone["content"])
	}
}

func TestExecRunGateResultIsData(t *testing.T) {
	r := New()

	// A passing gate: ok true, exit 0, and no Go error.
	out, err := r.Call(context.Background(), "exec.run", map[string]any{"cmd": "printf out; printf err >&2"})
	if err != nil {
		t.Fatalf("passing command should not error: %v", err)
	}
	if out["ok"] != true || out["exit_code"] != 0 {
		t.Errorf("passing gate = %v, want ok:true exit:0", out)
	}
	if out["stdout"] != "out" || out["stderr"] != "err" {
		t.Errorf("captured streams = %q / %q", out["stdout"], out["stderr"])
	}

	// The contract: a FAILING build is DATA, not a Go error — the engine
	// must route on it, not retry it as a transient action_error.
	out, err = r.Call(context.Background(), "exec.run", map[string]any{"cmd": "echo boom >&2; exit 3"})
	if err != nil {
		t.Fatalf("non-zero exit must be data, not error: %v", err)
	}
	if out["ok"] != false || out["exit_code"] != 3 {
		t.Errorf("failing gate = %v, want ok:false exit:3", out)
	}
	if !strings.Contains(out["stderr"].(string), "boom") {
		t.Errorf("stderr = %q, want it to carry the failure text", out["stderr"])
	}

	// A command that cannot LAUNCH is genuine (transient) infra failure.
	if _, err := r.Call(context.Background(), "exec.run", map[string]any{"cmd": "true", "cwd": "/no/such/dir"}); err == nil {
		t.Error("unreadable cwd should raise a (transient) error")
	}
}
