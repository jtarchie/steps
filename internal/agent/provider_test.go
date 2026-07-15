package agent

import (
	"strings"
	"testing"
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
}
