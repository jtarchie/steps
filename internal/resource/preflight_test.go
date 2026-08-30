package resource

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// preflightConfig builds a pipeline whose one resource is backed by the
// fixture MCP server, with tools whose schemas declare what they require —
// the thing preflight reads.
func preflightConfig(t *testing.T, mcp *config.MCPResourceConfig, put *config.Step) *config.Config {
	t.Helper()

	cfg := mcpFixtureConfig(t)
	cfg.ResourceTypes = []config.ResourceType{{
		Name:   "linear-issues",
		Config: config.ResourceTypeConfig{MCP: mcp},
	}}
	cfg.Resources = []config.Resource{{
		Name:   "eng-bugs",
		Type:   "linear-issues",
		Source: map[string]any{"team": "ENG"},
	}}

	if put != nil {
		cfg.Jobs = []config.Job{{Name: "react", Plan: []config.Step{*put}}}
	}

	return cfg
}

// preflightOnce runs preflight over the fixture's one resource. It does NOT
// reset the cache: the cache is keyed on the server's endpoint, and every
// fixture gets a fresh httptest server, so parallel tests cannot see each
// other's entries.
func preflightOnce(t *testing.T, cfg *config.Config) []config.Problem {
	t.Helper()

	return Preflight(context.Background(), cfg, nil, []string{"eng-bugs"}, nil)
}

func TestPreflightAcceptsAWorkingResource(t *testing.T) {
	t.Parallel()

	problems := preflightOnce(t, preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_issues"},
	}, nil))

	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
}

func TestPreflightNamesAToolTheServerDoesNotHave(t *testing.T) {
	t.Parallel()

	problems := preflightOnce(t, preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_isssues"},
	}, nil))

	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}

	// The available list is the whole point: a typo is unfixable from
	// "not found" alone.
	if !strings.Contains(problems[0].Detail, "list_isssues") || !strings.Contains(problems[0].Detail, "list_issues") {
		t.Errorf("detail = %q, want the missing tool and what the server does offer", problems[0].Detail)
	}
}

// TestPreflightCatchesAnUnsatisfiableCheck is the failure that motivated all
// of this: a `steps web` whose check tool requires an argument the resource
// never sends polls forever, logging the same error, enqueueing nothing.
func TestPreflightCatchesAnUnsatisfiableCheck(t *testing.T) {
	t.Parallel()

	problems := preflightOnce(t, preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "search_issues"},
	}, nil))

	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}

	if !strings.Contains(problems[0].Detail, "query") || !strings.Contains(problems[0].Detail, "team") {
		t.Errorf("detail = %q, want the required argument and what is actually sent", problems[0].Detail)
	}
}

func TestPreflightAcceptsRequiredArgsSuppliedByArgsMapping(t *testing.T) {
	t.Parallel()

	problems := preflightOnce(t, preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check: &config.MCPToolCall{
			Tool: "search_issues",
			Args: map[string]any{"query": "team:{{ .source.team }}"},
		},
	}, nil))

	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
}

// TestPreflightJudgesOutAgainstThePutsPayload: with no out.args:, the put's
// own params: ARE the arguments, so whether the tool's requirements are met
// is a question about the STEP, not the resource.
func TestPreflightJudgesOutAgainstThePutsPayload(t *testing.T) {
	t.Parallel()

	mcp := &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_issues"},
		Out:    &config.MCPToolCall{Tool: "post_issue"},
	}

	problems := preflightOnce(t, preflightConfig(t, mcp, &config.Step{
		Put:    "eng-bugs",
		Params: map[string]any{"text": "hi"}, // the tool wants `message`
	}))

	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "message") {
		t.Fatalf("problems = %+v, want the missing `message` argument", problems)
	}

	problems = preflightOnce(t, preflightConfig(t, mcp, &config.Step{
		Put:    "eng-bugs",
		Params: map[string]any{"message": "hi"},
	}))

	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none once the put sends what the tool requires", problems)
	}
}

// TestPreflightHandlesAPublishOnlyType: a type that only publishes declares
// no check:, and preflight judges the stages it actually has.
func TestPreflightHandlesAPublishOnlyType(t *testing.T) {
	t.Parallel()

	mcp := &config.MCPResourceConfig{
		Server: "test",
		Out:    &config.MCPToolCall{Tool: "post_issue"},
	}

	problems := preflightOnce(t, preflightConfig(t, mcp, &config.Step{
		Put:    "eng-bugs",
		Params: map[string]any{"message": "hi"},
	}))

	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}

	problems = preflightOnce(t, preflightConfig(t, mcp, &config.Step{
		Put:    "eng-bugs",
		Params: map[string]any{"text": "hi"},
	}))

	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "message") {
		t.Fatalf("problems = %+v, want the out stage still judged", problems)
	}
}

// TestPreflightIgnoresShellBackedResources: a shell check's correctness is
// whatever the command does, and preflight must not invent an opinion.
func TestPreflightIgnoresShellBackedResources(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		ResourceTypes: []config.ResourceType{{
			Name:   "git",
			Config: config.ResourceTypeConfig{Check: "true"},
		}},
		Resources: []config.Resource{{Name: "eng-bugs", Type: "git"}},
	}

	problems := preflightOnce(t, cfg)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
}

func TestPreflightReportsAnUnreachableServer(t *testing.T) {
	t.Parallel()

	cfg := preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_issues"},
	}, nil)
	cfg.MCPServers[0].Endpoint = "http://127.0.0.1:1/mcp"

	problems := preflightOnce(t, cfg)
	if len(problems) != 1 || !strings.Contains(problems[0].Detail, "eng-bugs") {
		t.Fatalf("problems = %+v, want one naming the resource that cannot reach its server", problems)
	}
}

// TestPreflightJudgesOnlyThePreflightedJobsPuts: whether THIS job can work
// cannot depend on how a job it is not running spells its own put. Before the
// scope was per-job, `steps run notify` refused to start because an unrelated
// `report` job sent the wrong params to a resource `notify` only gets.
func TestPreflightJudgesOnlyThePreflightedJobsPuts(t *testing.T) {
	t.Parallel()

	cfg := preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_issues"},
		Out:    &config.MCPToolCall{Tool: "post_issue"},
	}, nil)

	cfg.Jobs = []config.Job{
		{Name: "notify", Plan: []config.Step{{Get: "eng-bugs"}}},
		{Name: "report", Plan: []config.Step{{Put: "eng-bugs", Params: map[string]any{"text": "hi"}}}},
	}

	problems := Preflight(context.Background(), cfg, &cfg.Jobs[0], []string{"eng-bugs"}, nil)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none: `notify` only gets this resource", problems)
	}

	problems = Preflight(context.Background(), cfg, &cfg.Jobs[1], []string{"eng-bugs"}, nil)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want the one `report` actually causes", problems)
	}

	// A nil job is the `steps web` scope: it will run every job, so every
	// put in the pipeline is fair game.
	problems = Preflight(context.Background(), cfg, nil, []string{"eng-bugs"}, nil)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want the pipeline-wide put judged when no job is named", problems)
	}
}
