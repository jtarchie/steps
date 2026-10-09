package resource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/config"
)

// TestPreflightIgnoresTheOutToolOfAJobThatOnlyGets: a typo in an out: tool the job never publishes through must not block it.
func TestPreflightIgnoresTheOutToolOfAJobThatOnlyGets(t *testing.T) {
	t.Parallel()

	cfg := preflightConfig(t, &config.MCPResourceConfig{
		Server: "test",
		Check:  &config.MCPToolCall{Tool: "list_issues"},
		Out:    &config.MCPToolCall{Tool: "post_isue"},
	}, nil)
	cfg.Jobs = []config.Job{{Name: "notify", Plan: []config.Step{{Get: "eng-bugs"}}}}

	problems := Preflight(context.Background(), cfg, &cfg.Jobs[0], []string{"eng-bugs"}, nil)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none: `notify` never puts", problems)
	}
}

func TestToolListCacheTrustsAnEntryForItsWindow(t *testing.T) {
	t.Parallel()

	cache := &toolListCache{entries: map[string]toolListEntry{}}
	at := time.Unix(1000, 0)
	cache.store("k", toolListEntry{at: at})

	for name, tc := range map[string]struct {
		ttl  time.Duration
		now  time.Time
		want bool
	}{
		"fresh":              {time.Minute, at.Add(time.Second), true},
		"exactly the window": {time.Minute, at.Add(time.Minute), true},
		"past the window":    {time.Minute, at.Add(time.Minute + 1), false},
		"no window":          {0, at, false},
	} {
		if _, found := cache.lookup("k", tc.ttl, tc.now); found != tc.want {
			t.Errorf("%s: found = %v, want %v", name, found, tc.want)
		}
	}
}

func TestArgAdviceNamesTheKnobThatSuppliesTheArgument(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		stage string
		args  map[string]any
		want  string
	}{
		{"out", map[string]any{"team": "ENG"}, "add it to the resource type's mcp.out.args:"},
		{"out", nil, "the put's params: ARE the arguments"},
		{"check", nil, "the resource's source: IS the argument object"},
	} {
		if got := argAdvice(tc.stage, config.MCPToolCall{Tool: "t", Args: tc.args}); !strings.Contains(got, tc.want) {
			t.Errorf("%s with args %v: %q, want %q", tc.stage, tc.args, got, tc.want)
		}
	}
}

// TestListToolsNamesItsOwnDeadline: "did not answer" is reported for our deadline, and only for it.
func TestListToolsNamesItsOwnDeadline(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(silent.Close)
	t.Cleanup(func() { close(release) })

	cfg := &config.Config{MCPServers: []config.MCPServer{{Name: "test", Endpoint: silent.URL}}}

	_, err := listToolsCached(context.Background(), cfg, "test", &config.Preflight{Timeout: "200ms"})
	if err == nil || !strings.Contains(err.Error(), "did not answer within 200ms") {
		t.Errorf("err = %v, want the probe deadline named", err)
	}
}

func TestListToolsDoesNotCallARefusalATimeout(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{MCPServers: []config.MCPServer{{Name: "test", Endpoint: "http://127.0.0.1:1/mcp"}}}

	_, err := listToolsCached(context.Background(), cfg, "test", &config.Preflight{})
	if err == nil || strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want the refused connection, not a timeout", err)
	}
}

// TestListToolsDoesNotHoldACallersCancelAgainstTheServer: one Ctrl-C must not fail every later job touching a healthy server for the cache window.
func TestListToolsDoesNotHoldACallersCancelAgainstTheServer(t *testing.T) {
	t.Parallel()

	cfg := mcpFixtureConfig(t)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := listToolsCached(canceled, cfg, "test", &config.Preflight{})
	if err == nil || strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %v, want the cancel, not a timeout", err)
	}

	tools, err := listToolsCached(context.Background(), cfg, "test", &config.Preflight{})
	if err != nil || len(tools) == 0 {
		t.Errorf("after a canceled caller: %d tools, %v; want the healthy server's tools", len(tools), err)
	}
}

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
