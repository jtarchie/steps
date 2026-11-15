package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
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

func TestAnExportRefusesAValueQuotedInsideQuotesOfItsOwn(t *testing.T) {
	t.Parallel()

	for _, run := range []string{`echo "{{ .args.note | shellquote }}"`, `echo '{{ .args.note | shellquote }}'`} {
		err := CheckExportedCommands(config.MCPExport{Name: "shell", Tools: []config.ToolSpec{{Name: "say", Description: "d", Run: run}}})
		if err == nil || !strings.Contains(err.Error(), "inside quotes") {
			t.Errorf("%s = %v, want refused for sitting inside quotes", run, err)
		}
	}

	err := CheckExportedCommands(config.MCPExport{Name: "shell", Tools: []config.ToolSpec{{Name: "say", Description: "d", Run: `echo "fixed" {{ .args.note | shellquote }}`}}})
	if err != nil {
		t.Errorf("a bare shellquoted word beside a quoted literal was refused: %v", err)
	}
}

// Not t.Parallel(): TMPDIR is process-wide.
func TestACallThatCannotStartIsStillReportedToTheHook(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+"/missing")

	var reported []string

	hooks := ExportHooks{After: func(_ context.Context, tool string, names []string, ok bool, rule string) {
		reported = append(reported, tool+":"+strings.Join(names, ",")+":"+strconv.FormatBool(ok)+":"+rule)
	}}

	calls := callSpace{export: config.MCPExport{Name: "e"}, kinds: map[string]callKind{"say": commandCall}}
	handler := exportHandler("say", func(context.Context, map[string]any, toolEnv) map[string]any { return nil }, calls, hooks)

	result, err := handler(t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Arguments: []byte(`{"note":"x"}`)}})
	if err != nil || !result.IsError {
		t.Fatalf("handler = %+v, %v; want a tool error", result, err)
	}

	if len(reported) != 1 || reported[0] != "say:note:false:" {
		t.Errorf("hook saw %v, want the failed call reported once", reported)
	}
}

// An exported image: agent has no parent runner to share, so its call must bring one built through the venue: shell.NewRunner refuses a container with a tree (#246).
func TestAnExportedImageAgentIsHandedAContainerRunner(t *testing.T) {
	t.Parallel()

	cfg, err := config.Parse([]byte(`
agents:
- name: oncall
  source: { model: openrouter/x, api_key_env: K }
  image: alpine:3
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
mcp_exports:
- {name: a, access: 'true', tools: [{agent: oncall}]}
`), "test", nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var built []shell.RunnerSpec

	calls := newCallSpace(cfg, cfg.MCPExports[0], func(_ context.Context, spec shell.RunnerSpec) (shell.Runner, error) {
		built = append(built, spec)

		return shell.NewRunner(shell.RunnerSpec{})
	})

	env, done, err := calls.open(t.Context(), "oncall")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer done()

	if env.runner == nil || len(built) != 1 || built[0].Image != "alpine:3" || built[0].Cwd != env.dir {
		t.Errorf("open = runner %v, built %+v; want one alpine:3 runner over the call's directory %q", env.runner, built, env.dir)
	}
}

func TestAnExportRefusesAHeredocBecauseTheShellExpandsInsideIt(t *testing.T) {
	t.Parallel()

	export := config.MCPExport{Name: "shell", Tools: []config.ToolSpec{{Name: "note", Description: "d", Run: "cat <<EOF > note.txt\n{{ .args.note | shellquote }}\nEOF"}}}

	err := CheckExportedCommands(export)
	if err == nil || !strings.Contains(err.Error(), "heredoc") {
		t.Errorf("CheckExportedCommands = %v, want the heredoc refused", err)
	}
}
