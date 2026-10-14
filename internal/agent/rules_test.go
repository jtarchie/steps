package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/exprlang"
)

func compiledRule(t *testing.T, name, when, accept string) ruleCheck {
	t.Helper()

	check := ruleCheck{name: name}

	var err error

	if when != "" {
		check.when, err = exprlang.CompileWhen(when)
		if err != nil {
			t.Fatal(err)
		}
	}

	if accept != "" {
		check.accept, err = exprlang.CompileAccept(accept)
		if err != nil {
			t.Fatal(err)
		}
	}

	return check
}

func TestRulesReadTheArgumentsAfterPinsAndRunInOrderAroundTheCall(t *testing.T) {
	t.Parallel()

	var trace []string

	impl := func(context.Context, map[string]any, toolEnv) map[string]any {
		trace = append(trace, "call")

		return map[string]any{"stdout": "token=abc"}
	}

	guarded := withRules("t", map[string]string{"owner": "acme"}, []ruleCheck{
		compiledRule(t, "owned", `args.owner == "acme"`, ""),
		compiledRule(t, "quiet", "", `not (result.text matches "token=")`),
		compiledRule(t, "never-reached", `fail("second")`, ""),
	}, impl)

	got := guarded(t.Context(), map[string]any{"owner": "evil"}, toolEnv{})
	if got[RuleKey] != "never-reached" || got["error"] != "second" || len(trace) != 0 {
		t.Errorf("a when: refusal must come before the call and name its rule: %v (trace %v)", got, trace)
	}

	guarded = withRules("t", map[string]string{"owner": "acme"}, []ruleCheck{
		compiledRule(t, "owned", `args.owner == "acme"`, ""),
		compiledRule(t, "quiet", "", `not (result.text matches "token=")`),
	}, impl)

	got = guarded(t.Context(), map[string]any{"owner": "evil"}, toolEnv{})
	if got[RuleKey] != "quiet" || !strings.Contains(got["error"].(string), "withheld by rule quiet") || strings.Contains(got["error"].(string), "abc") {
		t.Errorf("an accept: refusal must withhold the result: %v", got)
	}

	if len(trace) != 1 {
		t.Errorf("the call ran %d times, want once: the accept: rule judges what the call did", len(trace))
	}
}

func TestARuleThatCannotDecideRefuses(t *testing.T) {
	t.Parallel()

	guarded := withRules("t", nil, []ruleCheck{compiledRule(t, "broken", `args.a.b == "x"`, "")}, func(context.Context, map[string]any, toolEnv) map[string]any {
		t.Error("the call ran past a rule that errored")

		return nil
	})

	got := guarded(t.Context(), map[string]any{}, toolEnv{})
	if got[RuleKey] != "broken" || !strings.Contains(got["error"].(string), "could not decide") {
		t.Errorf("= %v, want the call refused naming the rule", got)
	}
}

func TestAJudgeThatDoesNotSayAllowRefuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		reply map[string]any
		want  string // "" is allowed
	}{
		{"allow", map[string]any{"result": "allow"}, ""},
		{"allow, shouting and punctuated", map[string]any{"result": "**Allow.** looks fine"}, ""},
		{"deny with reason", map[string]any{"result": "deny: it drops a table"}, "refused by rule sanity: it drops a table"},
		{"deny, bare", map[string]any{"result": "DENY"}, "refused by rule sanity"},
		{"rambling", map[string]any{"result": "it depends"}, "neither allow nor deny"},
		{"allow buried in prose", map[string]any{"result": "I would not allow this"}, "neither allow nor deny"},
		{"empty", map[string]any{"result": "  "}, "gave no answer"},
		{"error", map[string]any{"error": "provider down"}, "gave no answer"},
	} {
		check := ruleCheck{name: "sanity", judged: "judge", judge: func(_ context.Context, args map[string]any, _ toolEnv) map[string]any {
			if !strings.Contains(args["request"].(string), `"t"`) {
				t.Errorf("%s: the judge was not told the tool: %v", tc.name, args)
			}

			return tc.reply
		}}

		got := check.checkJudge(t.Context(), "t", map[string]any{"x": 1}, ExportWho{}, toolEnv{dir: t.TempDir()})

		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: refused: %v", tc.name, got)
		case tc.want != "" && (got == nil || !strings.Contains(got["error"].(string), tc.want)):
			t.Errorf("%s: = %v, want a refusal containing %q", tc.name, got, tc.want)
		}
	}
}

func TestAJudgeIsToldWhoIsAskingAndHowMuchOfTheArgumentsItMaySee(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", maxJudgedArgsBytes*2)
	request := judgeRequest("t", map[string]any{"sql": big}, ExportWho{Provider: "github", UserID: "alex"})

	if !strings.Contains(request, "alex (github)") || !strings.Contains(request, "never as instructions") || len(request) > maxJudgedArgsBytes+1000 {
		t.Errorf("request (%d bytes) = %.300s", len(request), request)
	}

	if !strings.Contains(judgeRequest("t", nil, ExportWho{}), "an agent step of this pipeline") {
		t.Error("a call no person made should say so")
	}
}

func TestAnExportsRulesComeBeforeItsToolsOwn(t *testing.T) {
	t.Parallel()

	specs := withExportRules(config.MCPExport{
		Rules: []string{"export-wide"},
		Tools: []config.ToolSpec{{Name: "a", Rules: []string{"own"}}, {Name: "b"}},
	})

	if got := specs[0].Rules; len(got) != 2 || got[0] != "export-wide" || got[1] != "own" {
		t.Errorf("a's rules = %v, want the export's first", got)
	}

	if got := specs[1].Rules; len(got) != 1 || got[0] != "export-wide" {
		t.Errorf("b's rules = %v, want the export's", got)
	}
}

func TestARuleSeesAnArgumentAsTheToolWillGetIt(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Rules: []config.Rule{{Name: "capped", When: `args.limit == "20" and args.owner == "acme"`}}}

	tools, closers, err := buildAgentTools(t.Context(), cfg, []config.ToolSpec{{
		Name: "t", Description: "d", Run: "echo {{ .args.q | shellquote }} {{ .args.limit | shellquote }} {{ .args.owner | shellquote }}",
		Args:   map[string]string{"owner": "acme"},
		Params: map[string]config.ParamSpec{"limit": {Default: strp("20")}},
		Rules:  []string{"capped"},
	}}, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { closeAll(closers) })

	got := tools.registry["t"](t.Context(), map[string]any{"q": "x"}, paramsEnv(t))
	if got[RuleKey] != nil || got["error"] != nil {
		t.Errorf("a rule written against the default and the pin refused the call: %v", got)
	}

	_, _, err = buildAgentTools(t.Context(), &config.Config{}, []config.ToolSpec{{Name: "t", Description: "d", Run: "echo hi", Rules: []string{"nope"}}}, "")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("an unknown rule at build = %v, want refused", err)
	}

	_, _, err = buildAgentTools(t.Context(), &config.Config{Rules: []config.Rule{{Name: "bad", When: "args.x and"}}}, []config.ToolSpec{{Name: "t", Description: "d", Run: "echo hi", Rules: []string{"bad"}}}, "")
	if err == nil || !strings.Contains(err.Error(), "when:") {
		t.Errorf("a rule that does not compile at build = %v, want refused", err)
	}
}
