package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestCheckExecution(t *testing.T) {
	t.Parallel()

	matchErr := checkExecution("job \"x\"", []string{"a", "b"}, []string{"a", "b"})
	if matchErr != nil {
		t.Errorf("exact match should pass, got %v", matchErr)
	}

	err := checkExecution("job \"x\"", []string{"a", "b"}, []string{"a", "c"})
	if err == nil {
		t.Fatal("a mismatch should error")
	}

	if !strings.Contains(err.Error(), "want") || !strings.Contains(err.Error(), "got") {
		t.Errorf("mismatch error should show want/got diff, got %q", err)
	}

	// Order matters.
	reorderErr := checkExecution("job \"x\"", []string{"a", "b"}, []string{"b", "a"})
	if reorderErr == nil {
		t.Error("reordered execution should mismatch")
	}
}

func TestAssertMismatch(t *testing.T) {
	t.Parallel()

	code0, code1 := 0, 1
	want := "hello"

	tests := []struct {
		name     string
		assert   config.Assert
		stdout   string
		exitCode int
		match    bool
	}{
		{"code matches", config.Assert{Code: &code1}, "", 1, true},
		{"code mismatches", config.Assert{Code: &code0}, "", 1, false},
		{"stdout contains", config.Assert{Stdout: &want}, "say hello world", 0, true},
		{"stdout missing", config.Assert{Stdout: &want}, "goodbye", 0, false},
		{"both match", config.Assert{Stdout: &want, Code: &code1}, "hello", 1, true},
		{"code ok but stdout missing", config.Assert{Stdout: &want, Code: &code1}, "nope", 1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := assertMismatch(&tt.assert, tt.stdout, tt.exitCode, "")
			if tt.match && err != nil {
				t.Errorf("expected match, got %v", err)
			}

			if !tt.match && err == nil {
				t.Error("expected a mismatch error, got nil")
			}
		})
	}
}

func TestAssertFilesMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.MkdirAll(filepath.Join(dir, "answer"), 0o750)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "answer", "reply.md"), []byte("hi"), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	err = os.WriteFile(filepath.Join(dir, "answer", "empty.md"), nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		files []string
		match bool
	}{
		{"present and non-empty", []string{"answer/reply.md"}, true},
		{"missing", []string{"answer/missing.md"}, false},
		{"present but empty", []string{"answer/empty.md"}, false},
		{"directory, not a file", []string{"answer"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := config.AssertFilesMismatch(tt.files, dir)
			if tt.match && err != nil {
				t.Errorf("expected match, got %v", err)
			}

			if !tt.match && err == nil {
				t.Error("expected a mismatch error, got nil")
			}
		})
	}
}
