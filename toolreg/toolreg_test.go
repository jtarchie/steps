package toolreg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileReadConfinement(t *testing.T) {
	root := t.TempDir()
	err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("inside"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	err = os.WriteFile(secret, []byte("outside"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	r := New()
	out, err := r.Call(context.Background(), "file.read", map[string]any{"root": root, "path": "ok.txt"})
	if err != nil || out["content"] != "inside" {
		t.Fatalf("confined read failed: %v %v", out, err)
	}
	// Model-authored escapes must be refused, not resolved.
	for _, escape := range []string{"../secret.txt", "../../etc/passwd", "/etc/passwd"} {
		_, err := r.Call(context.Background(), "file.read", map[string]any{"root": root, "path": escape})
		if err == nil {
			t.Errorf("path %q should be refused", escape)
		}
	}
}

func TestDiffSplitEnrichment(t *testing.T) {
	root := t.TempDir()
	err := os.WriteFile(filepath.Join(root, "a.go"), []byte(strings.Repeat("x", 100)), 0o600)
	if err != nil {
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
	_, err = r.Call(context.Background(), "exec.run", map[string]any{"cmd": "true", "cwd": "/no/such/dir"})
	if err == nil {
		t.Error("unreadable cwd should raise a (transient) error")
	}
}

func TestHTTPGetSendsHeaders(t *testing.T) {
	// The server compares the header itself and reports a fixed verdict —
	// reflecting untrusted input back into the body would trip gosec.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tok-123" {
			_, _ = w.Write([]byte("authorized"))
			return
		}
		_, _ = w.Write([]byte("missing"))
	}))
	defer srv.Close()

	r := New()
	out, err := r.Call(context.Background(), "http.get", map[string]any{
		"url":     srv.URL,
		"headers": map[string]any{"Authorization": "Bearer tok-123"},
	})
	if err != nil {
		t.Fatalf("http.get with headers: %v", err)
	}
	if out["status"] != 200 {
		t.Errorf("status = %v, want 200", out["status"])
	}
	if out["body"] != "authorized" {
		t.Errorf("body = %q, want the header to have arrived verbatim", out["body"])
	}
}

func TestHTTPGetNoHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := New()
	out, err := r.Call(context.Background(), "http.get", map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("http.get without headers should still work: %v", err)
	}
	if out["status"] != 200 || out["body"] != "ok" {
		t.Errorf("out = %v, want status 200 body ok", out)
	}
}

func TestHTTPGetNon2xxIsData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	r := New()
	out, err := r.Call(context.Background(), "http.get", map[string]any{"url": srv.URL})
	if err != nil {
		t.Fatalf("non-2xx must be data, not error: %v", err)
	}
	if out["status"] != 404 || out["body"] != "nope" {
		t.Errorf("out = %v, want status 404 body nope (non-2xx is DATA)", out)
	}
}

func TestHTTPGetBadHeaderType(t *testing.T) {
	r := New()
	_, err := r.Call(context.Background(), "http.get", map[string]any{
		"url":     "http://127.0.0.1:1",
		"headers": map[string]any{"X-Bad": 5},
	})
	if err == nil || !strings.Contains(err.Error(), "must be a string") {
		t.Errorf("non-string header value should error naming the type, got: %v", err)
	}
}
