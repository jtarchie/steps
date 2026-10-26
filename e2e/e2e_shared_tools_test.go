package e2e

// Shared tools (#219): limits on a tool's parameters, and rules that check a
// call. Each test is one trip through cli.Run with a real MCP client at one end
// and a fake upstream at the other, like e2e_exports_test.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/testdocker"
)

// servedPipeline is a daemon holding a pipeline named ops, signed in as alex.
type servedPipeline struct {
	addr string
	dir  string
}

// serveOps sets pipeline (a whole YAML file, with a trivial job appended) on a fresh daemon and returns it. Not for t.Parallel(), for startWeb's reason.
func serveOps(t *testing.T, pipeline string) servedPipeline {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, "defaults:\n  preflight:\n    disabled: true\n"+pipeline+"\njobs:\n- name: build\n  plan:\n  - task: say\n    run: echo hi\n")

	served := startWeb(t, "--db", filepath.Join(dir, "daemon.db"), "--local-user", "alex:"+hashed(t, "a"))
	t.Cleanup(func() { served.stopIfRunning(t) })

	err := cli.Run([]string{"pipeline", "set", "-p", "ops", "-c", path, "-n", "--target", authTarget(served.addr, "alex", "a")})
	if err != nil {
		t.Fatalf("steps pipeline set: %v", err)
	}

	return servedPipeline{addr: served.addr, dir: dir}
}

// connect is an agent's session on one of the pipeline's exports, as alex.
func (s servedPipeline) connect(t *testing.T, export string) *sdkmcp.ClientSession {
	t.Helper()

	token := connectAgent(t, s.addr, export)

	return mcpSession(t, "http://"+s.addr+"/mcp/x/ops/"+export, token)
}

func callTool(t *testing.T, session *sdkmcp.ClientSession, name string, args map[string]any) *sdkmcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}

	return result
}

// reached is how many calls the upstream received for tool.
func reached(upstream *docMCPServer, tool string) int {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()

	return len(upstream.calls[tool])
}

func expectRefused(t *testing.T, session *sdkmcp.ClientSession, tool string, args map[string]any, want string) {
	t.Helper()

	got := callTool(t, session, tool, args)
	if !got.IsError || !strings.Contains(textOf(got), want) {
		t.Errorf("%s %v = %q, want a refusal saying %q", tool, args, textOf(got), want)
	}
}

func expectJudgeWasTold(t *testing.T, question string) {
	t.Helper()

	if !strings.Contains(question, `"db__run_query"`) || !strings.Contains(question, "select 1") || !strings.Contains(question, "alex (local)") {
		t.Errorf("the judge was not told what was being asked and by whom:\n%s", question)
	}
}

func marshalled(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return string(raw)
}

func publishedSchema(t *testing.T, session *sdkmcp.ClientSession, tool string) map[string]any {
	t.Helper()

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, offered := range tools.Tools {
		if offered.Name != tool {
			continue
		}

		raw, err := json.Marshal(offered.InputSchema)
		if err != nil {
			t.Fatal(err)
		}

		var schema map[string]any

		_ = json.Unmarshal(raw, &schema)

		return schema
	}

	t.Fatalf("tool %q is not offered", tool)

	return nil
}

// The limits reach the caller in the schema and bind it whether or not it read them; a refusal never reaches the upstream.
func TestParameterLimitsArePublishedAndEnforcedBeforeTheUpstream(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:       "search_issues",
		required:   []string{"query"},
		properties: map[string]string{"limit": "integer"},
		reply:      func(args map[string]any) string { return "searched " + args["query"].(string) },
	})

	served := serveOps(t, `
mcp_servers:
- name: github
  endpoint: `+upstream.URL+`
mcp_exports:
- name: support
  access: "true"
  tools:
  - mcp: github
    tool: search_issues
    params:
      query: { max_length: 20, pattern: '^[^;]*$' }
      limit: { max: 50, default: 20 }
`)

	session := served.connect(t, "support")

	properties := publishedSchema(t, session, "github__search_issues")["properties"].(map[string]any)
	query, limit := marshalled(t, properties["query"]), marshalled(t, properties["limit"])

	if !strings.Contains(query, `"maxLength":20`) || !strings.Contains(query, `"pattern"`) || !strings.Contains(limit, `"maximum":50`) || !strings.Contains(limit, `"default":20`) {
		t.Errorf("the caller was not shown the limits:\nquery %s\nlimit %s", query, limit)
	}

	for _, refused := range []map[string]any{
		{"query": strings.Repeat("q", 21)},
		{"query": "a;b"},
		{"query": "ok", "limit": 51},
		{"query": "ok", "limit": "many"},
	} {
		result := callTool(t, session, "github__search_issues", refused)
		if !result.IsError {
			t.Errorf("%v was not refused: %s", refused, textOf(result))
		}
	}

	if got := reached(upstream, "search_issues"); got != 0 {
		t.Fatalf("%d refused calls reached the upstream", got)
	}

	result := callTool(t, session, "github__search_issues", map[string]any{"query": "crash"})
	if result.IsError {
		t.Fatalf("a call inside the limits was refused: %s", textOf(result))
	}

	expectDocMCPCall(t, upstream, "search_issues", map[string]any{"query": "crash", "limit": float64(20)})
}

// A caller supplies values and only values: whatever it sends reaches the command as one literal word, and every call works in a directory of its own that is gone afterwards.
func TestAnExportedCommandReceivesWhatTheCallerSendsAsLiteralText(t *testing.T) {
	served := serveOps(t, `
mcp_exports:
- name: shell
  access: "true"
  tools:
  - name: say
    description: Print a note
    run: 'pwd; echo note={{ .args.note | shellquote }}; echo kept > marker; ls'
    params:
      note: { max_length: 40 }
`)

	session := served.connect(t, "shell")

	first := callTool(t, session, "say", map[string]any{"note": "$(id -u); echo injected `whoami`"})
	if first.IsError {
		t.Fatalf("the call failed: %s", textOf(first))
	}

	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}

	_ = json.Unmarshal([]byte(textOf(first)), &out)

	if !strings.Contains(out.Stdout, "note=$(id -u); echo injected `whoami`\n") || strings.Count(out.Stdout, "injected") != 1 {
		t.Errorf("the caller's text was interpreted, not passed through:\n%s", out.Stdout)
	}

	firstDir := strings.SplitN(out.Stdout, "\n", 2)[0]

	second := callTool(t, session, "say", map[string]any{"note": "again"})
	_ = json.Unmarshal([]byte(textOf(second)), &out)

	if secondDir := strings.SplitN(out.Stdout, "\n", 2)[0]; secondDir == firstDir {
		t.Errorf("two calls shared the directory %s", firstDir)
	}

	if strings.Count(out.Stdout, "marker") != 1 {
		t.Errorf("the second call saw the first call's files:\n%s", out.Stdout)
	}

	_, err := os.Stat(firstDir)
	if !os.IsNotExist(err) {
		t.Errorf("the first call's directory outlived the call: %v", err)
	}

	tooLong := callTool(t, session, "say", map[string]any{"note": strings.Repeat("n", 41)})
	if !tooLong.IsError {
		t.Errorf("params: did not apply to an exported command: %s", textOf(tooLong))
	}
}

// A command sees only the variables it names, and their values never reach the caller: not on stdout, not on stderr, not escaped into the JSON result.
func TestAnExportedCommandSeesOnlyTheVariablesItNamesAndGetsThemBackMasked(t *testing.T) {
	const value = `tok<5ecret>&"q`

	t.Setenv("EXPORT_TOKEN", value)
	t.Setenv("EXPORT_OTHER", "other-value")

	served := serveOps(t, `
mcp_exports:
- name: shell
  access: "true"
  tools:
  - name: authed
    description: Uses the token
    env: [EXPORT_TOKEN]
    run: 'echo token=$EXPORT_TOKEN; echo other=${EXPORT_OTHER:-unset}; echo bearer $EXPORT_TOKEN >&2; exit 3'
  - name: plain
    description: Names nothing
    run: 'echo token=${EXPORT_TOKEN:-unset}'
`)

	session := served.connect(t, "shell")

	authed := textOf(callTool(t, session, "authed", nil))
	if strings.Contains(authed, "5ecret") || strings.Count(authed, "***") != 2 {
		t.Errorf("the token reached the caller, or was not masked where it was printed: %s", authed)
	}

	if !strings.Contains(authed, "other=unset") {
		t.Errorf("a variable the tool did not name reached its command: %s", authed)
	}

	if plain := textOf(callTool(t, session, "plain", nil)); !strings.Contains(plain, "token=unset") {
		t.Errorf("a tool that names nothing saw another tool's variable: %s", plain)
	}
}

// A caller's value that would reach the shell as syntax is refused where the pipeline is set, where somebody reads the reason — not discovered by a caller.
func TestAnExportedCommandThatLeavesAValueUnquotedIsRefusedAtSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.yml")
	writePipelineFile(t, path, `
mcp_exports:
- name: shell
  access: "true"
  tools:
  - name: say
    description: Print a note
    run: echo {{ .args.note }}
jobs:
- name: build
  plan:
  - task: say
    run: echo hi
`)

	err := cli.Run([]string{"validate", path})
	if err == nil || !strings.Contains(err.Error(), "unquoted") || !strings.Contains(err.Error(), `mcp_exports "shell"`) {
		t.Errorf("steps validate = %v, want the unquoted value named", err)
	}
}

// An export that names an image runs its commands there, as the container's user and with no network if it says so, each call in a fresh container over a directory of its own.
func TestAnExportedCommandRunsInTheContainerTheExportNames(t *testing.T) {
	testdocker.Require(t)
	t.Setenv("TMPDIR", testdocker.MountableDir(t))

	served := serveOps(t, `
mcp_exports:
- name: shell
  access: "true"
  image: `+dockerE2EImage+`
  network: none
  tools:
  - name: whereami
    description: Say where this runs
    run: 'cat /etc/alpine-release; echo kept > marker; ls; wget -q -T 2 -O- http://example.com >/dev/null 2>&1 && echo online || echo offline'
`)

	session := served.connect(t, "shell")

	for range 2 {
		var out struct {
			Stdout string `json:"stdout"`
		}

		_ = json.Unmarshal([]byte(textOf(callTool(t, session, "whereami", nil))), &out)

		if !strings.Contains(out.Stdout, "3.") || !strings.Contains(out.Stdout, "offline") || strings.Count(out.Stdout, "marker") != 1 {
			t.Fatalf("the command did not run in a fresh, offline alpine container:\n%s", out.Stdout)
		}
	}
}

// A when: rule refuses before anything goes upstream and says why; it reads the arguments as the tool will get them (pins applied) and who is asking.
func TestAWhenRuleRefusesACallBeforeItReachesTheUpstreamAndIsRecordedByName(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:     "search_issues",
		required: []string{"owner", "query"},
		reply:    func(args map[string]any) string { return "searched " + args["query"].(string) },
	})

	served := serveOps(t, `
mcp_servers:
- name: github
  endpoint: `+upstream.URL+`
rules:
- name: pinned-org-and-alex
  when: args.owner == "acme" and who.UserID == "alex" and who.Provider == "local"
- name: lowercase-words
  when: 'args.query matches "^[a-z ]+$" ? true : fail("ask in lowercase words only")'
- name: never
  when: args.query != "forbidden"
mcp_exports:
- name: support
  access: "true"
  tools:
  - mcp: github
    tool: search_issues
    args: { owner: acme }
    rules: [pinned-org-and-alex, lowercase-words, never]
`)

	session := served.connect(t, "support")

	expectRefused(t, session, "github__search_issues", map[string]any{"query": "HELP $(id)", "owner": "evil"}, "ask in lowercase words only")
	expectRefused(t, session, "github__search_issues", map[string]any{"query": "forbidden"}, "refused by rule never")

	if got := reached(upstream, "search_issues"); got != 0 {
		t.Fatalf("%d refused calls reached the upstream", got)
	}

	allowed := callTool(t, session, "github__search_issues", map[string]any{"query": "crash on start", "owner": "evil"})
	if allowed.IsError {
		t.Fatalf("a call every rule allows was refused: %s", textOf(allowed))
	}

	expectDocMCPCall(t, upstream, "search_issues", map[string]any{"owner": "acme", "query": "crash on start"})

	refusalsAreRecordedByRuleName(t, served)
}

func refusalsAreRecordedByRuleName(t *testing.T, served servedPipeline) {
	t.Helper()

	_, activity := authBody(t, served.addr, "alex", "a", "/activity?action=mcp+refused")
	if !strings.Contains(activity, "lowercase-words") || !strings.Contains(activity, "never") || strings.Contains(activity, "HELP") || strings.Count(activity, "mcp refused") < 2 {
		t.Errorf("the activity log should name the rules that refused, and never what was asked:\n%s", activity)
	}

	_, page := authBody(t, served.addr, "alex", "a", "/p/ops/mcp/exports/support")
	if !strings.Contains(page, "2 refused by a rule today") || !strings.Contains(page, "pinned-org-and-alex") {
		t.Errorf("the export's page should list its rules and count today's refusals:\n%s", page)
	}
}

// An accept: rule withholds a result the caller must not see, however it was produced, and the rule on the export covers every tool in it.
func TestAnAcceptRuleWithholdsWhatCameBack(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:     "read_note",
		required: []string{"name"},
		reply: func(args map[string]any) string {
			if args["name"] == "keys" {
				return "-----BEGIN PRIVATE KEY----- abc"
			}

			return "a harmless note"
		},
	})

	served := serveOps(t, `
mcp_servers:
- name: notes
  endpoint: `+upstream.URL+`
rules:
- name: no-secrets
  accept: not (result.text matches "(?i)private key|api[_-]?key")
mcp_exports:
- name: support
  access: "true"
  rules: [no-secrets]
  tools:
  - mcp: notes
    tool: read_note
  - name: leak
    description: Prints a key
    run: echo api_key=hunter2
`)

	session := served.connect(t, "support")

	if got := callTool(t, session, "notes__read_note", map[string]any{"name": "ok"}); got.IsError || !strings.Contains(textOf(got), "harmless") {
		t.Errorf("a clean result = %+v", textOf(got))
	}

	for _, withheld := range []*sdkmcp.CallToolResult{
		callTool(t, session, "notes__read_note", map[string]any{"name": "keys"}),
		callTool(t, session, "leak", nil),
	} {
		text := textOf(withheld)
		if !withheld.IsError || !strings.Contains(text, "withheld by rule no-secrets") || strings.Contains(text, "PRIVATE KEY") || strings.Contains(text, "hunter2") {
			t.Errorf("a result matching the rule reached the caller: %+v", text)
		}
	}
}

// A judged_by rule asks one of the pipeline's agents, which is told what is being asked and answers allow or deny; a rambling or failing judge is no.
func TestAJudgedRuleAsksAnAgentAndAnythingButAllowRefuses(t *testing.T) {
	upstream := startDocMCPServer(t, docMCPTool{
		name:     "run_query",
		required: []string{"sql"},
		reply:    func(args map[string]any) string { return "ran " + args["sql"].(string) },
	})

	judged := make(chan string, 8)

	fake := newRoutedFakeLLM(t, func(req capturedRequest) turn {
		var question string

		for _, msg := range req.Messages {
			if msg.Role == "user" {
				question = msg.Content
			}
		}

		judged <- question

		switch {
		case strings.Contains(question, "drop table"):
			return says("Deny. That destroys data.")
		case strings.Contains(question, "ramble"):
			return says("Well, it depends on many things.")
		default:
			return says("allow")
		}
	})

	t.Setenv("STEPS_TEST_AGENT_API_KEY", "test")

	served := serveOps(t, `
mcp_servers:
- name: db
  endpoint: `+upstream.URL+`
agents:
- name: triage-judge
  source: { model: openai/test-model, endpoint: `+fake.URL+`/v1/, api_key_env: STEPS_TEST_AGENT_API_KEY }
  system: You decide whether a database call may go ahead.
rules:
- name: sanity
  judged_by: triage-judge
mcp_exports:
- name: support
  access: "true"
  tools:
  - mcp: db
    tool: run_query
    rules: [sanity]
`)

	session := served.connect(t, "support")

	if got := callTool(t, session, "db__run_query", map[string]any{"sql": "select 1"}); got.IsError {
		t.Fatalf("a call the judge allowed was refused: %s", textOf(got))
	}

	expectJudgeWasTold(t, <-judged)

	expectRefused(t, session, "db__run_query", map[string]any{"sql": "drop table users"}, "refused by rule sanity: That destroys data.")
	expectRefused(t, session, "db__run_query", map[string]any{"sql": "ramble"}, "neither allow nor deny")

	if got := reached(upstream, "run_query"); got != 1 {
		t.Errorf("%d calls reached the upstream, want only the allowed one", got)
	}
}
