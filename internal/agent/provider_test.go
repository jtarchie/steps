package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestLookupAPIKey(t *testing.T) {
	t.Run("required and set succeeds", func(t *testing.T) {
		t.Setenv("STEPS_TEST_KEY_1", "secret")

		got, err := lookupAPIKey("STEPS_TEST_KEY_1", true)
		if err != nil {
			t.Fatal(err)
		}

		if got != "secret" {
			t.Errorf("got %q, want %q", got, "secret")
		}
	})

	t.Run("required and unset errors", func(t *testing.T) {
		_, err := lookupAPIKey("STEPS_TEST_KEY_DOES_NOT_EXIST", true)
		if err == nil {
			t.Error("expected an error for a required but unset env var")
		}
	})

	t.Run("required with empty envVar name errors", func(t *testing.T) {
		_, err := lookupAPIKey("", true)
		if err == nil {
			t.Error("expected an error for a required key with no envVar name")
		}
	})

	t.Run("not required and unset returns empty, no error", func(t *testing.T) {
		got, err := lookupAPIKey("", false)
		if err != nil {
			t.Fatal(err)
		}

		if got != "" {
			t.Errorf("got %q, want empty string", got)
		}
	})
}

func TestBuildSystemMessage(t *testing.T) {
	t.Parallel()

	t.Run("custom persona is preserved and dir noted", func(t *testing.T) {
		t.Parallel()

		got := buildSystemMessage("You are a terse reviewer.", "/work/prs")
		if !strings.HasPrefix(got, "You are a terse reviewer.") {
			t.Errorf("persona not preserved: %q", got)
		}

		if !strings.Contains(got, "/work/prs") {
			t.Errorf("working directory not mentioned: %q", got)
		}
	})

	t.Run("empty persona falls back to the default", func(t *testing.T) {
		t.Parallel()

		got := buildSystemMessage("", "/work")
		if !strings.HasPrefix(got, defaultAgentPersona) {
			t.Errorf("expected the default persona, got %q", got)
		}
	})

	t.Run("context blocks are NOT in the system message", func(t *testing.T) {
		t.Parallel()

		got := buildSystemMessage("persona", "/work")
		if strings.Contains(got, "<context") || strings.Contains(got, "</context>") {
			t.Errorf("system message should not contain context blocks: %q", got)
		}
	})
}

func TestLoadContextBlocks(t *testing.T) {
	t.Parallel()

	t.Run("nil paths resolve to nil", func(t *testing.T) {
		t.Parallel()

		blocks, err := loadContextBlocks(t.TempDir(), nil, 0)
		if err != nil || blocks != nil {
			t.Errorf("got (%v, %v), want (nil, nil)", blocks, err)
		}
	})

	t.Run("reads declared files from the workspace", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()

		err := os.MkdirAll(filepath.Join(dir, "repo"), 0o750)
		if err != nil {
			t.Fatal(err)
		}

		err = os.WriteFile(filepath.Join(dir, "repo", "CLAUDE.md"), []byte("follow CLAUDE.md exactly\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		blocks, err := loadContextBlocks(dir, []string{"repo/CLAUDE.md"}, 0)
		if err != nil {
			t.Fatal(err)
		}

		if len(blocks) != 1 || blocks[0].path != "repo/CLAUDE.md" || blocks[0].content != "follow CLAUDE.md exactly\n" {
			t.Errorf("got %+v", blocks)
		}
	})
}

func TestLoadContextBlocksErrors(t *testing.T) {
	t.Parallel()

	t.Run("missing file is a preparation error", func(t *testing.T) {
		t.Parallel()

		_, err := loadContextBlocks(t.TempDir(), []string{"repo/MISSING.md"}, 0)
		if err == nil {
			t.Fatal("expected an error for a missing context file")
		}

		if !strings.Contains(err.Error(), "repo/MISSING.md") {
			t.Errorf("error should name the declared path, got %v", err)
		}
	})

	t.Run("paths escaping the workspace are rejected", func(t *testing.T) {
		t.Parallel()

		_, err := loadContextBlocks(t.TempDir(), []string{"../../etc/passwd"}, 0)
		if err == nil {
			t.Fatal("expected an error for an escaping context path")
		}
	})
}

// TestContextPathTruncatesInsteadOfFailing pins the degradation an oversized
// context file gets.
//
// It used to fail the step. The live PR-review pipeline hit exactly that: a
// `pr/pr.diff` that had grown past the limit killed the run at preparation —
// a correct path, authored by an operator who does not control how large the
// pull request under review happens to be.
func TestContextPathTruncatesInsteadOfFailing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	big := strings.Repeat("x", maxReadFileBytes+5000)

	err := os.WriteFile(filepath.Join(dir, "big.diff"), []byte(big), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := loadContextBlocks(dir, []string{"big.diff"}, config.DefaultMaxContextBytes)
	if err != nil {
		t.Fatalf("an oversized context path failed the step: %v", err)
	}

	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}

	if !strings.Contains(blocks[0].content, "[truncated:") {
		t.Error("the truncation is silent; the model must be told there is more")
	}

	if !strings.Contains(blocks[0].content, "read_file") {
		t.Error("the notice does not say how to reach the rest")
	}
}

// TestContextPathZeroLimitHandsOverTheWholeFile pins max_context_bytes: 0 —
// the explicit "no ceiling". It is the case a plain int could not express:
// before the dial became a pointer, 0 was indistinguishable from an omitted
// field and silently took the 100KB default.
func TestContextPathZeroLimitHandsOverTheWholeFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	big := strings.Repeat("x", config.DefaultMaxContextBytes+5000)

	err := os.WriteFile(filepath.Join(dir, "big.diff"), []byte(big), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	blocks, err := loadContextBlocks(dir, []string{"big.diff"}, 0)
	if err != nil {
		t.Fatalf("loadContextBlocks: %v", err)
	}

	if got := len(blocks[0].content); got != len(big) {
		t.Errorf("content = %d bytes, want the whole %d — max_context_bytes: 0 capped a file it should not have", got, len(big))
	}

	if strings.Contains(blocks[0].content, "[truncated:") {
		t.Error("an uncapped file carries a truncation notice")
	}
}

// TestContextPathHonoursAConfiguredLimit pins that max_context_bytes: actually
// moves the ceiling, in both directions from the default.
func TestContextPathHonoursAConfiguredLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := os.WriteFile(filepath.Join(dir, "diff"), []byte(strings.Repeat("x", 5000)), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	// Under the default, so it arrives whole.
	blocks, err := loadContextBlocks(dir, []string{"diff"}, 0)
	if err != nil || strings.Contains(blocks[0].content, "[truncated:") {
		t.Errorf("a 5KB file was truncated under the default ceiling: err=%v", err)
	}

	// A tighter ceiling truncates it.
	blocks, err = loadContextBlocks(dir, []string{"diff"}, 1000)
	if err != nil {
		t.Fatalf("loadContextBlocks: %v", err)
	}

	if !strings.Contains(blocks[0].content, "[truncated:") {
		t.Error("a configured ceiling below the file size did not truncate")
	}
}
