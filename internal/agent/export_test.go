package agent

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestAnExportRefusesACommandThatLeavesACallersValueUnquoted(t *testing.T) {
	t.Parallel()

	export := config.MCPExport{Name: "shell", Tools: []config.ToolSpec{{Name: "say", Description: "d", Run: "echo {{ .args.note }}"}}}

	err := CheckExportedCommands(export)
	if err == nil || !strings.Contains(err.Error(), `tool "say"`) || !strings.Contains(err.Error(), "unquoted") {
		t.Errorf("CheckExportedCommands = %v, want the tool and the unquoted value named", err)
	}

	// Building the export checks again: a pipeline loaded by any route, not only a set, is held to it.
	_, err = NewExportServer(t.Context(), &config.Config{Name: "p"}, export, ExportHooks{})
	if err == nil || !strings.Contains(err.Error(), "unquoted") {
		t.Errorf("NewExportServer = %v, want it refused", err)
	}

	export.Tools[0].Run = "echo {{ .args.note | shellquote }}"

	err = CheckExportedCommands(export)
	if err != nil {
		t.Errorf("a quoted value was refused: %v", err)
	}
}
