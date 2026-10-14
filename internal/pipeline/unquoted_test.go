package pipeline

import (
	"slices"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestUnquotedShellValuesReachEveryPosition(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Agents: []config.Agent{{Name: "a", Tools: []config.ToolSpec{{Name: "t", Run: "echo {{ .args.x }}"}}}},
		Tasks:  []config.Task{{Name: "k", Fix: &config.FixSpec{Tools: []config.ToolSpec{{Name: "f", Run: "echo {{ .args.y }}"}}}}},
		Jobs: []config.Job{{Name: "j", Plan: []config.Step{{
			Agent: "a",
			Tools: []config.ToolSpec{{Name: "s", Run: "echo {{ .args.z }}"}},
			Fix:   &config.FixSpec{Tools: []config.ToolSpec{{Name: "sf", Run: "echo {{ .args.w }}"}}},
		}}}},
		ResourceTypes: []config.ResourceType{{Name: "r", Config: config.ResourceTypeConfig{
			Check: "echo {{ .version.v | shellquote }}",
			In:    "echo {{ .params.p }}",
			Out:   "echo {{ .source.s }}",
		}}},
	}

	const advice = " reaches the shell unquoted; pipe it through shellquote"

	want := []string{
		`resource type "r" in: {{.params.p}}` + advice,
		`resource type "r" out: {{.source.s}}` + advice,
		`agent "a" tool "t" run: {{.args.x}}` + advice,
		`task "k" fix tool "f" run: {{.args.y}}` + advice,
		`job "j" step 0 tool "s" run: {{.args.z}}` + advice,
		`job "j" step 0 fix tool "sf" run: {{.args.w}}` + advice,
	}

	got := UnquotedShellValues(cfg)
	if !slices.Equal(got, want) {
		t.Errorf("got\n%q\nwant\n%q", got, want)
	}
}
