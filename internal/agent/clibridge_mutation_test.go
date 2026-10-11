package agent

import (
	"context"
	"reflect"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/genai"
)

// MCP lets a client omit arguments for a tool that takes none (the Go client fills in {}, a non-Go cli need not), and that is a call, not malformed JSON.
func TestCLIBridgeCallWithNoArguments(t *testing.T) {
	t.Parallel()

	ran := false
	impl := func(context.Context, map[string]any, toolEnv) map[string]any {
		ran = true

		return map[string]any{"exit_code": 0}
	}

	bridge, err := newCLIBridge(t.Context(), bridgeConversation(nil, map[string]toolImpl{}, nil), nil)
	if err != nil {
		t.Fatalf("newCLIBridge: %v", err)
	}

	t.Cleanup(func() { _ = bridge.Close(t.Context()) })

	call := bridge.handler("list_files", impl, toolEnv{})

	result, err := call(t.Context(), &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "list_files"}})
	if err != nil {
		t.Fatalf("a call with no arguments was refused: %v", err)
	}

	if result.IsError || !ran {
		t.Errorf("a call with no arguments did not run the tool (ran=%v, result=%+v)", ran, result)
	}
}

func TestCLIBridgeSchemaCarriesOnlyWhatWasDeclared(t *testing.T) {
	t.Parallel()

	bare := genaiSchemaToJSON(&genai.Schema{Type: genai.TypeObject})
	if want := map[string]any{"type": "object"}; !reflect.DeepEqual(bare, want) {
		t.Errorf("a bare object schema converted to %v, want %v — an empty description, enum, properties or required is not the same schema", bare, want)
	}

	full := genaiSchemaToJSON(&genai.Schema{
		Type:        genai.TypeObject,
		Description: "the arguments",
		Properties: map[string]*genai.Schema{
			"mode": {Type: genai.TypeString, Enum: []string{"fast", "slow"}},
		},
		Required: []string{"mode"},
	})

	want := map[string]any{
		"type":        "object",
		"description": "the arguments",
		"properties": map[string]any{
			"mode": map[string]any{"type": "string", "enum": []string{"fast", "slow"}},
		},
		"required": []string{"mode"},
	}
	if !reflect.DeepEqual(full, want) {
		t.Errorf("schema converted to %v, want %v", full, want)
	}
}
