package e2e

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/events"
)

// pinnedArgsPipeline grants list_faults on the fixture server with pins.
func pinnedArgsPipeline(llm, mcp, pins string) string {
	return `
defaults:
  preflight:
    disabled: true

mcp_servers:
- name: hb
  endpoint: ` + mcp + `

agents:
- name: oncall
  source: { model: openai/test-model, endpoint: ` + llm + `/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  tools:
  - mcp: hb
    tool: list_faults
    args: ` + pins + `

jobs:
- name: triage
  plan:
  - agent: oncall
    inputs: []
    messages:
      - What is failing?
`
}

func startPinFixture(t *testing.T) *docMCPServer {
	t.Helper()

	return startDocMCPServer(t, docMCPTool{
		name: "list_faults", required: []string{"a"},
		properties: map[string]string{"n": "integer", "q": "string"},
		reply:      func(map[string]any) string { return "[]" },
	})
}

// TestMCPPinnedArgs carries a pin across every seam it crosses — YAML, the
// schema the model is offered, the merge, and the wire to the MCP server.
func TestMCPPinnedArgs(t *testing.T) {
	srv := startPinFixture(t)
	fake := newFakeLLM(t,
		callsTool("hb__list_faults", map[string]any{"a": "other", "A": "sneaky", "q": "x"}),
		says("nothing is failing"),
	)

	path := writePipeline(t, t.TempDir(), pinnedArgsPipeline(fake.URL, srv.URL, `{ a: pinned, n: "7" }`))
	mustRun(t, "run", path)

	schema := offeredSchema(t, fake.request(1), "hb__list_faults")

	if _, has := schema.Properties["q"]; !has {
		t.Fatalf("offered schema = %+v, want q still offered", schema)
	}

	for _, pinned := range []string{"a", "n"} {
		if _, has := schema.Properties[pinned]; has {
			t.Errorf("offered properties %v include the pinned %q", schema.Properties, pinned)
		}
	}

	if len(schema.Required) != 0 {
		t.Errorf("offered required = %v, want the pinned a gone from it", schema.Required)
	}

	// n arrives as the integer its schema declares; A, a case variant of a
	// pin that the tool does not declare, is dropped rather than forwarded.
	if got, want := srv.lastCall(t, "list_faults"), map[string]any{"a": "pinned", "n": float64(7), "q": "x"}; !reflect.DeepEqual(got, want) {
		t.Errorf("server received %v, want %v", got, want)
	}

	note := pinOverrideNote(t, path)
	if !strings.Contains(note, `"A", "a"`) || strings.Contains(note, "other") || strings.Contains(note, "sneaky") {
		t.Errorf("override note = %q, want the keys named and the model's values withheld", note)
	}
}

type offeredToolSchema struct {
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required"`
}

// offeredSchema returns the parameters schema req offered the model for tool.
func offeredSchema(t *testing.T, req capturedRequest, tool string) offeredToolSchema {
	t.Helper()

	for _, offered := range req.Tools {
		if offered.Function.Name != tool {
			continue
		}

		var schema offeredToolSchema

		err := json.Unmarshal(offered.Function.Parameters, &schema)
		if err != nil {
			t.Fatalf("parameters %s: %v", offered.Function.Parameters, err)
		}

		return schema
	}

	t.Fatalf("the model was not offered %q", tool)

	return offeredToolSchema{}
}

// pinOverrideNote returns the step note the run recorded for a model
// overriding a pin.
func pinOverrideNote(t *testing.T, path string) string {
	t.Helper()

	st := openStoreFor(t, path)

	runs, err := st.ListRuns(t.Context(), "triage", 1)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns: %v (%d runs)", err, len(runs))
	}

	rows, err := st.RunEvents(t.Context(), runs[0].ID, 0, 500)
	if err != nil {
		t.Fatalf("RunEvents: %v", err)
	}

	for _, row := range rows {
		if row.Type == events.TypeStepNote && strings.Contains(row.Text, "pinned values used") {
			return row.Text
		}
	}

	t.Fatal("run_events holds no pin-override note")

	return ""
}

// TestMCPPinnedArgsRefusedBeforeTheModel: a pin that cannot bind fails the
// step at preparation — before a single model turn — and never echoes the
// pinned value.
func TestMCPPinnedArgsRefusedBeforeTheModel(t *testing.T) {
	cases := []struct {
		name string
		pins string
		want string
	}{
		{"undeclared key", `{ zzz: SECRETVALUE }`, `pins "zzz", which the server does not declare (it declares: a, n, q)`},
		{"value not of the declared type", `{ n: seven }`, `pins "n" as integer, but the pinned value is not an integer`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := startPinFixture(t)
			fake := newFakeLLM(t, says("should never be asked"))

			path := writePipeline(t, t.TempDir(), pinnedArgsPipeline(fake.URL, srv.URL, tc.pins))

			err := cli.Run([]string{"run", path})
			if err == nil {
				t.Fatal("the run succeeded with a pin that cannot bind")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("run error = %q, want it to contain %q", err, tc.want)
			}

			if strings.Contains(err.Error(), "SECRETVALUE") || strings.Contains(err.Error(), "seven") {
				t.Errorf("run error = %q echoes the pinned value", err)
			}

			if n := fake.requestCount(); n != 0 {
				t.Errorf("the model was asked %d time(s); a pin that cannot bind must stop the step first", n)
			}
		})
	}
}
