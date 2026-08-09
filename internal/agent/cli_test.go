package agent

import (
	"context"
	"slices"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/jtarchie/steps/internal/config"
)

// argValue returns the value following flag in an argument vector, or "".
func argValue(args []string, flag string) string {
	at := slices.Index(args, flag)
	if at < 0 || at+1 >= len(args) {
		return ""
	}

	return args[at+1]
}

// cliPrepared builds a prepared step with the given granted tool names,
// enough for the argument builder.
func cliPrepared(t *testing.T, toolNames []string, verdicts []string) preparedAgentStep {
	t.Helper()

	decls := make([]*genai.FunctionDeclaration, 0, len(toolNames))
	registry := map[string]toolImpl{}

	for _, name := range toolNames {
		decls = append(decls, &genai.FunctionDeclaration{Name: name, Parameters: &genai.Schema{Type: genai.TypeObject}})
		registry[name] = func(_ context.Context, _ map[string]any, _ toolEnv) map[string]any {
			return map[string]any{"exit_code": 0}
		}
	}

	conv := bridgeConversation(decls, registry, nil)
	conv.system = "You are a reviewer."
	conv.prompt = "Review the diff."

	return preparedAgentStep{
		step: config.Step{Agent: "reviewer", Verdicts: verdicts},
		ri: config.ResolvedInvocation{
			AgentName: "reviewer",
			CLI:       "claude",
			ModelName: "sonnet",
			MaxTurns:  12,
			Attempts:  1,
		},
		conv: conv,
	}
}

func TestCLIArgs(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, []string{"read_file", "run_shell", "count_lines"}, nil)
	args := cliArgs(prepared, cliRuntimes["claude"], "/tmp/mcp.json")

	for flag, want := range map[string]string{
		"--model":                "sonnet",
		"--max-turns":            "12",
		"--mcp-config":           "/tmp/mcp.json",
		"--append-system-prompt": "You are a reviewer.",
		"--output-format":        "stream-json",
	} {
		if got := argValue(args, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}

	// Without --strict-mcp-config the CLI would also load the user's own MCP
	// servers, handing the step tools the pipeline never granted.
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Error("--strict-mcp-config is missing; the tool grant would not be a limit")
	}

	if !slices.Contains(args, "--print") {
		t.Error("--print is missing; the cli would try to run interactively")
	}
}

func TestCLIToolPermissions(t *testing.T) {
	t.Parallel()

	prepared := cliPrepared(t, []string{"read_file", "run_shell", "count_lines"}, nil)
	args := cliArgs(prepared, cliRuntimes["claude"], "/tmp/mcp.json")

	allowed := strings.Split(argValue(args, "--allowedTools"), ",")
	denied := strings.Split(argValue(args, "--disallowedTools"), ",")

	// Granted built-ins become the CLI's own native tools.
	for _, want := range []string{"Read", "Bash"} {
		if !slices.Contains(allowed, want) {
			t.Errorf("allowed = %v, want it to contain %q", allowed, want)
		}
	}

	// A custom tool has no native equivalent; it reaches the CLI through the
	// bridge, under the bridge's namespaced name.
	if !slices.Contains(allowed, "mcp__steps__count_lines") {
		t.Errorf("allowed = %v, want the bridged custom tool", allowed)
	}

	// The ungranted built-ins must be DENIED, not merely omitted: a CLI's
	// read-only tools need no permission in non-interactive mode, so leaving
	// Grep off the allow-list would not withhold it.
	for _, want := range []string{"Write", "Edit", "Grep"} {
		if !slices.Contains(denied, want) {
			t.Errorf("denied = %v, want it to contain ungranted %q", denied, want)
		}
	}

	if slices.Contains(allowed, "Write") {
		t.Error("write_file was never granted but Write is allowed")
	}

	// Capabilities no grant can describe are denied unconditionally.
	for _, want := range []string{"Task", "WebFetch", "WebSearch"} {
		if !slices.Contains(denied, want) {
			t.Errorf("denied = %v, want it to contain %q", denied, want)
		}
	}
}

// TestCLIRuntimesCoverProviders keeps the two halves of the CLI tables in
// step: config knows which "@name" sources load, this package knows how to
// invoke them, and a CLI present in one but not the other would resolve at
// load and fail at the step.
func TestCLIRuntimesCoverProviders(t *testing.T) {
	t.Parallel()

	for _, name := range config.CLIProviderNames() {
		runtime, ok := cliRuntimes[name]
		if !ok {
			t.Errorf("config knows cli %q but internal/agent has no runtime for it", name)

			continue
		}

		if len(runtime.natives) == 0 {
			t.Errorf("cli %q maps no built-in tools to natives", name)
		}
	}

	for name := range cliRuntimes {
		if !slices.Contains(config.CLIProviderNames(), name) {
			t.Errorf("internal/agent has a runtime for cli %q, which config does not recognize", name)
		}
	}
}

// TestCLINativeMappingCoversBuiltins pins that every built-in tool a grant can
// name has a native equivalent. A built-in with no mapping would silently fall
// through to the bridge, which works but duplicates a capability the CLI
// already has — the failure this catches is a NEW built-in being added without
// deciding which side runs it.
func TestCLINativeMappingCoversBuiltins(t *testing.T) {
	t.Parallel()

	for name := range builtinAgentTools("") {
		if _, mapped := cliRuntimes["claude"].natives[name]; !mapped {
			t.Errorf("built-in %q has no native claude equivalent; map it or decide it belongs on the bridge", name)
		}
	}
}

func TestRenderCLIPrompt(t *testing.T) {
	t.Parallel()

	conv := agentConversation{
		recap:         "RECAP: the run so far.",
		contextBlocks: []contextBlock{{path: "repo/NOTES.md", content: "some notes"}},
		prompt:        "Review the diff.",
	}

	rendered := renderCLIPrompt(conv)

	// Same content, same order as the HTTP path's synthetic tool exchanges —
	// there is just no transcript to fabricate them into here.
	recapAt := strings.Index(rendered, "RECAP")
	blockAt := strings.Index(rendered, "repo/NOTES.md")
	promptAt := strings.Index(rendered, "Review the diff.")

	if recapAt < 0 || blockAt < 0 || promptAt < 0 {
		t.Fatalf("rendered prompt is missing a section:\n%s", rendered)
	}

	if recapAt >= blockAt || blockAt >= promptAt {
		t.Errorf("sections are out of order (recap %d, block %d, prompt %d):\n%s", recapAt, blockAt, promptAt, rendered)
	}

	if !strings.Contains(rendered, "some notes") {
		t.Error("context block content did not reach the prompt")
	}
}

func TestMergeCLITrajectory(t *testing.T) {
	t.Parallel()

	streamed := []recordedToolCall{
		{name: "Read", args: map[string]any{"file_path": "a.go"}, ok: true},
		{name: "mcp__steps__verdict", args: map[string]any{"choice": "approve"}, ok: true},
	}

	bridged := []recordedToolCall{
		{name: "verdict", args: map[string]any{"choice": "approve"}, ok: true},
		{name: "count_lines", args: map[string]any{"path": "a.go"}, ok: true},
	}

	merged := mergeCLITrajectory(streamed, bridged)

	// The verdict appears in both records and must not be double-counted.
	verdicts := 0

	for _, call := range merged {
		if call.name == "mcp__steps__verdict" {
			verdicts++
		}
	}

	if verdicts != 1 {
		t.Errorf("verdict appears %d times in %+v, want once", verdicts, merged)
	}

	// A bridged call the stream never mentioned definitely happened — the
	// parent executed it.
	if !slices.ContainsFunc(merged, func(call recordedToolCall) bool { return call.name == "mcp__steps__count_lines" }) {
		t.Errorf("merged = %+v, want the bridge-only call to survive", merged)
	}
}
