package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/shell"
)

func intp(n int) *int           { return &n }
func strp(s string) *string     { return &s }
func floatp(f float64) *float64 { return &f }

func paramsEnv(t *testing.T) toolEnv {
	t.Helper()

	dir := t.TempDir()

	runner, err := shell.NewRunner(shell.RunnerSpec{Cwd: dir})
	if err != nil {
		t.Fatal(err)
	}

	return toolEnv{dir: dir, runner: runner}
}

func buildParamsTool(t *testing.T, spec config.ToolSpec) (agentTools, map[string]any) {
	t.Helper()

	tools, closers, err := buildAgentTools(t.Context(), &config.Config{}, []config.ToolSpec{spec}, "")
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}

	t.Cleanup(func() { closeAll(closers) })

	raw, err := json.Marshal(declInputSchema(tools.decls.FunctionDeclarations[0]))
	if err != nil {
		t.Fatal(err)
	}

	var schema map[string]any

	_ = json.Unmarshal(raw, &schema)

	return tools, schema
}

func TestCustomToolParamsArePublishedAndDefaultMakesAnArgumentOptional(t *testing.T) {
	t.Parallel()

	tools, schema := buildParamsTool(t, config.ToolSpec{
		Name: "file", Description: "d", Run: "echo {{ .args.title | shellquote }} {{ .args.severity | shellquote }}",
		Params: map[string]config.ParamSpec{
			"title":    {MaxLength: intp(5), Pattern: "^[a-z]+$"},
			"severity": {Enum: []string{"low", "high"}, Default: strp("low")},
		},
	})

	properties := schema["properties"].(map[string]any)
	title := properties["title"].(map[string]any)
	severity := properties["severity"].(map[string]any)

	if title["maxLength"] != float64(5) || title["pattern"] != "^[a-z]+$" || severity["default"] != "low" || len(severity["enum"].([]any)) != 2 {
		t.Errorf("limits missing from the schema: %v", schema)
	}

	if required := schema["required"].([]any); len(required) != 1 || required[0] != "title" {
		t.Errorf("required = %v, want only title: a default makes severity optional", required)
	}

	callsAgainstTheLimits(t, tools.registry["file"])
}

func callsAgainstTheLimits(t *testing.T, impl toolImpl) {
	t.Helper()

	got := impl(t.Context(), map[string]any{"title": "crash"}, paramsEnv(t))
	if got["error"] != nil || !strings.Contains(got["stdout"].(string), "crash low") {
		t.Errorf("a call with severity left out = %v, want the default sent", got)
	}

	for _, args := range []map[string]any{{"title": "toolongtitle"}, {"title": "Caps"}, {"title": "ok", "severity": "urgent"}, {"title": 7}} {
		refused := impl(t.Context(), args, paramsEnv(t))

		message, _ := refused["error"].(string)
		if message == "" {
			t.Errorf("%v was not refused: %v", args, refused)
		}

		if strings.Contains(message, "toolongtitle") || strings.Contains(message, "urgent") {
			t.Errorf("the refusal quotes the value back: %q", message)
		}
	}
}

func TestParamsNameTheArgumentTheyRefuse(t *testing.T) {
	t.Parallel()

	tools, _ := buildParamsTool(t, config.ToolSpec{
		Name: "file", Description: "d", Run: "echo {{ .args.title | shellquote }}",
		Params: map[string]config.ParamSpec{"title": {MaxLength: intp(2)}},
	})

	got := tools.registry["file"](t.Context(), map[string]any{"title": "abc"}, paramsEnv(t))
	if got["error"] != "file: title must be at most 2 characters (it was 3)" {
		t.Errorf("error = %v", got["error"])
	}
}

func TestParamsTheToolCannotHonourAreRefusedAtBuild(t *testing.T) {
	t.Parallel()

	for name, spec := range map[string]config.ToolSpec{
		"unknown argument": {Name: "t", Description: "d", Run: "echo hi", Params: map[string]config.ParamSpec{"x": {MaxLength: intp(1)}}},
	} {
		_, _, err := buildAgentTools(t.Context(), &config.Config{}, []config.ToolSpec{spec}, "")
		if err == nil || !strings.Contains(err.Error(), "not an argument it takes") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestNumericParamsKeepTheirType(t *testing.T) {
	t.Parallel()

	prop := map[string]any{"type": "integer"}

	limit, err := publishParam("t", "limit", config.ParamSpec{Min: floatp(1), Max: floatp(50), Default: strp("20")}, prop)
	if err != nil {
		t.Fatal(err)
	}

	if prop["default"] != int64(20) || prop["minimum"] != 1.0 || prop["maximum"] != 50.0 || limit.fill != int64(20) {
		t.Errorf("schema = %v, fill = %v", prop, limit.fill)
	}

	numericCallsAreChecked(t, limit)
	numericLimitsFitTheirKind(t)
}

func numericCallsAreChecked(t *testing.T, limit paramLimit) {
	t.Helper()

	for value, want := range map[any]string{0.0: "at least 1", 51.0: "at most 50", 2.5: "whole number", "many": "must be a number"} {
		if got := checkParam(limit, value); !strings.Contains(got, want) {
			t.Errorf("checkParam(%v) = %q, want %q", value, got, want)
		}
	}

	if got := checkParam(limit, 7.0); got != "" {
		t.Errorf("checkParam(7) = %q, want it accepted", got)
	}
}

func numericLimitsFitTheirKind(t *testing.T) {
	t.Helper()

	_, err := publishParam("t", "limit", config.ParamSpec{MaxLength: intp(3)}, map[string]any{"type": "integer"})
	if err == nil || !strings.Contains(err.Error(), "use min/max") {
		t.Errorf("max_length on an integer = %v", err)
	}

	_, err = publishParam("t", "q", config.ParamSpec{Min: floatp(3)}, map[string]any{"type": "string"})
	if err == nil || !strings.Contains(err.Error(), "min/max do not apply") {
		t.Errorf("min on a string = %v", err)
	}

	_, err = publishParam("t", "q", config.ParamSpec{Default: strp("abc")}, map[string]any{"type": "integer"})
	if err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Errorf("a non-numeric default on an integer = %v", err)
	}
}
