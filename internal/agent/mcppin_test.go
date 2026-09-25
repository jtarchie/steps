package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/events"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}

	return string(data)
}

func pinTool(schema any) *sdkmcp.Tool {
	return &sdkmcp.Tool{Name: "list_faults", InputSchema: schema}
}

func pinSpec(args map[string]string) config.ToolSpec {
	return config.ToolSpec{MCP: "hb", MCPTool: "list_faults", Args: args}
}

func TestPinMCPArgsStripsSchemaAndConverts(t *testing.T) {
	t.Parallel()

	original := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{"type": "integer"},
			"ratio":      map[string]any{"type": []any{"number", "null"}},
			"open":       map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}}},
			"env":        map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
			"q":          map[string]any{"type": "string"},
		},
		"required": []any{"project_id", "q"},
	}

	before := mustMarshal(t, original)

	schema, pins, err := pinMCPArgs(pinSpec(map[string]string{
		"project_id": "307", "ratio": "0.5", "open": "true", "env": "12",
	}), pinTool(original))
	if err != nil {
		t.Fatalf("pinMCPArgs: %v", err)
	}

	after := mustMarshal(t, original)
	if before != after {
		t.Errorf("tool.InputSchema was mutated:\n%s\n%s", before, after)
	}

	got, _ := schema.(map[string]any)
	props, _ := got["properties"].(map[string]any)

	if len(props) != 1 || props["q"] == nil {
		t.Errorf("properties = %v, want only q", props)
	}

	if !reflect.DeepEqual(got["required"], []any{"q"}) {
		t.Errorf("required = %v, want [q]", got["required"])
	}

	want := map[string]any{"project_id": int64(307), "ratio": 0.5, "open": true, "env": "12"}
	if !reflect.DeepEqual(pins.values, want) {
		t.Errorf("values = %#v, want %#v", pins.values, want)
	}

	if pins.tool != "hb__list_faults" {
		t.Errorf("tool = %q, want the model-facing name", pins.tool)
	}
}

func TestPinMCPArgsNoPinsLeavesSchemaAlone(t *testing.T) {
	t.Parallel()

	original := map[string]any{"type": "object"}

	schema, pins, err := pinMCPArgs(pinSpec(nil), pinTool(original))
	if err != nil || pins != nil {
		t.Fatalf("pinMCPArgs = %v, %v, want no pins and no error", pins, err)
	}

	if !reflect.DeepEqual(schema, original) {
		t.Errorf("schema = %v, want the original", schema)
	}
}

func TestPinMCPArgsRefusals(t *testing.T) {
	t.Parallel()

	declared := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"project_id": map[string]any{"type": "integer"},
			"page":       map[string]any{"type": "integer"},
			"ratio":      map[string]any{"type": "number"},
			"open":       map[string]any{"type": "boolean"},
			"filter":     map[string]any{"type": "object"},
			"tags":       map[string]any{"type": []any{"array", "null"}},
		},
	}

	cases := []struct {
		name   string
		schema any
		args   map[string]string
		want   string
	}{
		{"undeclared key", declared, map[string]string{"project_idd": "SECRET"},
			`pins "project_idd", which the server does not declare (it declares: filter, open, page, project_id, ratio, tags). Run: steps mcp tools <pipeline> hb`},
		{"not an integer", declared, map[string]string{"page": "SECRET"}, `pins "page" as integer, but the pinned value is not an integer`},
		{"not a number", declared, map[string]string{"ratio": "SECRET"}, `pins "ratio" as number, but the pinned value is not a number`},
		{"not finite", declared, map[string]string{"ratio": "NaN"}, `pins "ratio" as number`},
		{"not a boolean", declared, map[string]string{"open": "SECRET"}, `pins "open" as boolean`},
		{"object", declared, map[string]string{"filter": "SECRET"}, `pins "filter" as object, but the pinned value is not an object`},
		{"array", declared, map[string]string{"tags": "SECRET"}, `pins "tags" as array, but the pinned value is not an array`},
		{"no properties", map[string]any{"type": "object"}, map[string]string{"a": "SECRET"}, "declares no properties to pin"},
		{"unreadable schema", func() {}, map[string]string{"a": "SECRET"}, "declares no properties to pin"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := pinMCPArgs(pinSpec(tc.args), pinTool(tc.schema))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}

			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("err = %v, must not echo the pinned value", err)
			}
		})
	}
}

// noteRecorder captures every step note published under the context it returns.
func noteRecorder(t *testing.T) (context.Context, func() []string) {
	t.Helper()

	bus := events.New(nil)
	t.Cleanup(bus.Close)

	var (
		mu    sync.Mutex
		notes []string
	)

	cancel := bus.Observe(func(event events.Event) {
		if event.Type == events.TypeStepNote {
			mu.Lock()
			notes = append(notes, event.Text)
			mu.Unlock()
		}
	})
	t.Cleanup(cancel)

	return events.WithBus(context.Background(), bus), func() []string {
		mu.Lock()
		defer mu.Unlock()

		return append([]string(nil), notes...)
	}
}

func TestMCPToolImplMergesPins(t *testing.T) {
	t.Parallel()

	pins := &mcpPins{
		tool:     "hb__list_faults",
		values:   map[string]any{"project_id": int64(307)},
		declared: map[string]bool{"project_id": true, "PROJECT_ID": true, "q": true},
	}

	client := &fakeMCPClient{result: &sdkmcp.CallToolResult{}}
	impl := mcpToolImpl(client, "list_faults", maxToolOutputBytes, pins)

	ctx, notes := noteRecorder(t)
	impl(ctx, map[string]any{
		"project_id": "999-injected", "Project_Id": "sneaky", "PROJECT_ID": "declared", "q": "x",
	}, toolEnv{})

	want := map[string]any{"project_id": int64(307), "PROJECT_ID": "declared", "q": "x"}
	if !reflect.DeepEqual(client.gotArgs, want) {
		t.Errorf("CallTool args = %#v, want %#v", client.gotArgs, want)
	}

	got := notes()
	if len(got) != 1 || !strings.Contains(got[0], `hb__list_faults: model supplied pinned argument(s) "Project_Id", "project_id"`) {
		t.Fatalf("notes = %q, want one override note naming both keys", got)
	}

	if strings.Contains(got[0], "injected") || strings.Contains(got[0], "sneaky") {
		t.Errorf("note = %q, must not echo the model's values", got[0])
	}
}

func TestMCPToolImplPinsWithoutOverrideSaysNothing(t *testing.T) {
	t.Parallel()

	pins := &mcpPins{tool: "hb__list_faults", values: map[string]any{"project_id": int64(307)}, declared: map[string]bool{"project_id": true, "q": true}}
	client := &fakeMCPClient{result: &sdkmcp.CallToolResult{}}

	ctx, notes := noteRecorder(t)
	mcpToolImpl(client, "list_faults", maxToolOutputBytes, pins)(ctx, map[string]any{"q": "x"}, toolEnv{})

	if want := map[string]any{"project_id": int64(307), "q": "x"}; !reflect.DeepEqual(client.gotArgs, want) {
		t.Errorf("CallTool args = %#v, want %#v", client.gotArgs, want)
	}

	if got := notes(); len(got) != 0 {
		t.Errorf("notes = %q, want none", got)
	}
}

func TestBuildMCPToolsPinned(t *testing.T) {
	t.Parallel()

	srv := newCountingMCPServer(t)
	cfg := &config.Config{MCPServers: []config.MCPServer{srv.server()}}

	built, closer, err := buildAgentTools(context.Background(), cfg,
		[]config.ToolSpec{{MCP: "test", MCPTool: "search_issues", Args: map[string]string{"query": "pinned"}}}, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}
	defer closeAll(closer)

	schema := mustMarshal(t, built.decls.FunctionDeclarations[0].ParametersJsonSchema)
	if strings.Contains(schema, "query") {
		t.Errorf("schema = %s, want query stripped", schema)
	}

	built.registry["test__search_issues"](context.Background(), map[string]any{}, toolEnv{})

	if calls := *srv.echoCalled; len(calls) != 1 || calls[0]["query"] != "pinned" {
		t.Errorf("server received %v, want query=pinned", calls)
	}

	_, _, err = buildAgentTools(context.Background(), cfg,
		[]config.ToolSpec{{MCP: "test", MCPTool: "search_issues", Args: map[string]string{"zzz": "x"}}}, "")
	if err == nil || !strings.Contains(err.Error(), `pins "zzz", which the server does not declare (it declares: query)`) {
		t.Fatalf("err = %v, want the undeclared pin refused at preparation", err)
	}
}
