package agent

import (
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// TestToolDescriptionPrefersTheEmbeddedFile pins which source wins: the embedded library is canonical, and the Go constant is only for a tool that has no file there.
func TestToolDescriptionPrefersTheEmbeddedFile(t *testing.T) {
	t.Parallel()

	embedded, err := config.ReadBuiltinToolDescription(config.AskUserBuiltinName)
	if err != nil || embedded == "" {
		t.Fatalf("the fixture needs an embedded description for %s: %q, %v", config.AskUserBuiltinName, embedded, err)
	}

	if got := toolDescription(config.AskUserBuiltinName, "fallback"); got != embedded {
		t.Errorf("toolDescription(%s) = %q, want the embedded file", config.AskUserBuiltinName, got)
	}

	if got := toolDescription("no_such_tool", "fallback"); got != "fallback" {
		t.Errorf("toolDescription(no_such_tool) = %q, want the fallback", got)
	}
}
