package web

import (
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

var (
	configJump = regexp.MustCompile(`href="(/p/[^"#]+/config/[0-9a-f]+)#([^"]+)"`)
	anyTag     = regexp.MustCompile(`<[^>]*>`)
)

// landConfigJump follows the one config link on page that carries a fragment and returns the text of the block it lands on: the link and the id are built in different files, so only following one to the other proves they agree.
func landConfigJump(t *testing.T, fetch func(string) (int, string), page, wantFragment string) string {
	t.Helper()

	link := configJump.FindStringSubmatch(page)
	if link == nil {
		t.Fatalf("no config link with a fragment:\n%s", page)
	}

	fragment, err := url.PathUnescape(html.UnescapeString(link[2]))
	if err != nil {
		t.Fatalf("fragment %q: %v", link[2], err)
	}

	if fragment != wantFragment {
		t.Fatalf("config link lands on %q, want %q", fragment, wantFragment)
	}

	code, config := fetch(link[1])
	if code != http.StatusOK {
		t.Fatalf("%s = %d", link[1], code)
	}

	return configBlockText(t, config, fragment)
}

// configBlockText is the source text inside the block carrying id, tags stripped.
func configBlockText(t *testing.T, page, id string) string {
	t.Helper()

	open := `<span class="cfgblock" id="` + html.EscapeString(id) + `">`

	_, rest, found := strings.Cut(page, open)
	if !found {
		t.Fatalf("the config page has no block with id %q:\n%s", id, page)
	}

	depth, end := 1, 0

	for depth > 0 {
		next := strings.Index(rest[end:], "<")
		if next < 0 {
			t.Fatalf("block %q never closes", id)
		}

		end += next

		switch {
		case strings.HasPrefix(rest[end:], "</span>"):
			depth--
		case strings.HasPrefix(rest[end:], "<span"):
			depth++
		}

		if depth > 0 {
			end++
		}
	}

	return html.UnescapeString(anyTag.ReplaceAllString(rest[:end], ""))
}

func recordServed(t *testing.T, pipeline *Pipeline) string {
	t.Helper()

	revision := pipeline.Config().Revision

	err := pipeline.Store.RecordRevision(t.Context(), revision.SHA, revision.Source, revision.Includes)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	return revision.SHA
}

// TestEntityPagesJumpToTheirConfigBlock: a page about one declared thing opens the configuration AT that thing, where the link used to be absent or to open the file at line one.
func TestEntityPagesJumpToTheirConfigBlock(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	sha := recordServed(t, pipeline)

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "ran", JobName: "deploy", ConfigSHA: sha})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	fetch := func(target string) (int, string) { return get(t, server, target) }

	for _, tc := range []struct{ page, fragment, block string }{
		{"/p/demo/jobs/build/detail", "job-build", "  - name: build\n    plan:\n      - get: repo\n      - task: compile\n        run: \"true\""},
		{"/p/demo/runs/ran", "job-deploy", "  - name: deploy\n    plan:\n      - get: repo\n        passed: [build]\n      - task: ship\n        run: \"true\""},
		{"/p/demo/resources/repo", "resource-repo", "  - name: repo\n    type: mock\n    source: {}"},
	} {
		t.Run(tc.fragment, func(t *testing.T) {
			t.Parallel()

			code, page := fetch(tc.page)
			if code != http.StatusOK {
				t.Fatalf("%s = %d", tc.page, code)
			}

			if got := landConfigJump(t, fetch, page, tc.fragment); got != tc.block {
				t.Errorf("%s lands on\n%q\nwant\n%q", tc.page, got, tc.block)
			}
		})
	}
}

// TestMCPServersJumpToTheirConfigBlock: each server row links its own entry, in a file whose list items sit at column zero.
func TestMCPServersJumpToTheirConfigBlock(t *testing.T) {
	t.Parallel()

	server, pipeline, _ := mcpServerPipeline(t, nil)
	sha := recordServed(t, pipeline)

	_, page := get(t, server, "/p/demo/mcp")

	row := regexp.MustCompile(`(?s)<tr id="mcp-gopls">.*?</tr>`).FindString(page)
	if !strings.Contains(row, `href="/p/demo/config/`+sha+`#mcp-server-gopls"`) {
		t.Fatalf("the gopls row does not link its config entry:\n%s", row)
	}

	_, config := get(t, server, "/p/demo/config/"+sha)

	if got, want := configBlockText(t, config, "mcp-server-gopls"), "- name: gopls\n  command: steps-no-such-mcp-server"; got != want {
		t.Errorf("gopls block is\n%q\nwant\n%q", got, want)
	}

	// The last entry of a section ends before the blank line and the next section, and an agent is a target too.
	if got, want := configBlockText(t, config, "mcp-server-perstep"), "- name: perstep\n  command: sh\n  cwd: repo"; got != want {
		t.Errorf("perstep block is\n%q\nwant\n%q", got, want)
	}

	if got := configBlockText(t, config, "agent-triager"); !strings.HasPrefix(got, "- name: triager\n") || !strings.HasSuffix(got, "    tool: list_issues") {
		t.Errorf("triager block is\n%q", got)
	}
}

// TestTheExportPageJumpsToItsConfigBlock: what an export offers is decided in the file, so its page links the entry.
func TestTheExportPageJumpsToItsConfigBlock(t *testing.T) {
	t.Parallel()

	server, _ := exportServer(t)
	pipeline := server.pipelines[0]
	recordServed(t, pipeline)

	fetch := func(target string) (int, string) {
		rec := serve(t, server, http.MethodGet, target, "", "alex")

		return rec.Code, rec.Body.String()
	}

	_, page := fetch("/p/ops/mcp/exports/support")

	if got := landConfigJump(t, fetch, page, "mcp-export-support"); !strings.Contains(got, "name: support") {
		t.Errorf("the export page lands on\n%q", got)
	}
}

// TestConfigBlocksLeaveTheSourceVerbatim: the page's promise is the recorded bytes, so wrapping blocks may add tags and nothing else — through a block scalar holding a fence, a comment between entries, an escaped name, and a one-line flow list no block can wrap.
func TestConfigBlocksLeaveTheSourceVerbatim(t *testing.T) {
	t.Parallel()

	const source = `# a pipeline
resources:
  - name: "a&b <c>"
    type: mock
  # about weekly
  - name: weekly
    type: cron

resource_types: [{name: mock}, {name: cron}]
jobs:
  - name: build
    plan:
      - task: say
        run: |
          cat <<'EOF'
          ` + "```" + `
          # not a comment
          EOF

  - name: last
    plan: []
`

	body := string(highlightConfig(source, configLinker{}))

	if got := html.UnescapeString(anyTag.ReplaceAllString(body, "")); got != source {
		t.Fatalf("the wrapped source is not the source:\n%q\nwant\n%q", got, source)
	}

	page := "<pre>" + body + "</pre>"

	for id, want := range map[string]string{
		"resource-a&b <c>": "  - name: \"a&b <c>\"\n    type: mock",
		"resource-weekly":  "  - name: weekly\n    type: cron",
		"job-build":        "  - name: build\n    plan:\n      - task: say\n        run: |\n          cat <<'EOF'\n          ```\n          # not a comment\n          EOF",
		"job-last":         "  - name: last\n    plan: []",
	} {
		if got := configBlockText(t, page, id); got != want {
			t.Errorf("block %q is\n%q\nwant\n%q", id, got, want)
		}
	}

	if strings.Contains(body, `id="resourcetype-`) {
		t.Errorf("two entries on one line cannot each be a block:\n%s", body)
	}

	if got := configURL("demo", "abc", "resource", "a&b <c>"); got != "/p/demo/config/abc#resource-a&b%20%3Cc%3E" {
		t.Errorf("configURL = %q", got)
	}
}

// TestAConfigThatWillNotParseIsStillShown: a recorded source is shown whatever it holds; only the landing places are lost.
func TestAConfigThatWillNotParseIsStillShown(t *testing.T) {
	t.Parallel()

	const source = "jobs:\n  - name: [unclosed\n"

	if got := html.UnescapeString(anyTag.ReplaceAllString(string(highlightConfig(source, configLinker{})), "")); got != source {
		t.Errorf("got %q, want the source", got)
	}
}

// TestTheConfigPageNumbersItsLines: a gutter beside the file, one number per line, and outside the <pre> the source sits in so a reader copying the file does not copy the numbers.
func TestTheConfigPageNumbersItsLines(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	const sha = "eeee111122223333"

	err := pipeline.Store.RecordRevision(t.Context(), sha, "jobs:\n  - name: a\n    plan: []\n", map[string]string{"ci/task.yml": "run: one\nrun: two"})
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, page := get(t, server, "/p/demo/config/"+sha)

	for _, want := range []string{
		`<pre class="json code lineno" aria-hidden="true">1` + "\n2\n3</pre><pre",
		`<pre class="json code lineno" aria-hidden="true">1` + "\n2</pre><pre",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the config page lacks the gutter %q:\n%s", want, page)
		}
	}
}

// TestJobHistoryLinksEachRunsConfig: a run's row opens the configuration THAT run executed, at this job, and says so when it is not the one being served — two runs a row apart may have been told different things.
func TestJobHistoryLinksEachRunsConfig(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	current := recordServed(t, pipeline)
	older := strings.Repeat("a", 40)

	err := pipeline.Store.RecordRevision(t.Context(), older, "jobs:\n  - name: build\n    plan: []\n", nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	for id, sha := range map[string]string{"now": current, "then": older, "unpinned": ""} {
		err = pipeline.Store.StartRun(t.Context(), store.NewRun{ID: id, JobName: "build", ConfigSHA: sha})
		if err != nil {
			t.Fatalf("StartRun %s: %v", id, err)
		}
	}

	_, page := get(t, server, "/p/demo/jobs/build/detail")

	rows := map[string]string{}
	for _, row := range regexp.MustCompile(`(?s)<tr>\s*<td><a class="hash" href="/p/demo/runs/(\w+)".*?</tr>`).FindAllStringSubmatch(page, -1) {
		rows[row[1]] = row[0]
	}

	for id, want := range map[string]struct {
		link    string
		differs bool
	}{
		"now":      {`href="/p/demo/config/` + current + `#job-build"`, false},
		"then":     {`href="/p/demo/config/` + older + `#job-build"`, true},
		"unpinned": {"", false},
	} {
		row := rows[id]

		if got := regexp.MustCompile(`href="[^"]*/config/[^"]*"`).FindString(row); got != want.link {
			t.Errorf("run %s: config link, want %q:\n%s", id, want.link, row)
		}

		if got := strings.Contains(row, "≠ current"); got != want.differs {
			t.Errorf("run %s: marked as differing = %v, want %v:\n%s", id, got, want.differs, row)
		}
	}

	_, config := get(t, server, "/p/demo/config/"+older)
	if got := configBlockText(t, config, "job-build"); got != "  - name: build\n    plan: []" {
		t.Errorf("the older revision's block is %q", got)
	}
}

const nestedSource = `resource_types:
  - name: mock
resources:
  - name: weekly
    type: mock
  - name: clock
    type: time
agents:
  - name: reviewer
    system_file: prompts/review.md
mcp_servers:
  - name: linear
    command: linear-mcp
    auth: { type: mock }
jobs:
  - name: build
    ensure:
      task: sweep
      run: "true"
    plan:
      - get: weekly
      - do:
          - task: compile
            run_file: ci/compile.sh
          - task: lint
            run_file: ci/absent.sh
        on_failure:
          task: report
          run: "true"
      - in_parallel:
          limit: 2
          steps:
            - task: unit
              run: "true"
            - try:
                task: flaky
                run: "true"
          # after the fan-out

      - agent: reviewer
        tools:
          - mcp: linear
          - mcp: nowhere
      - do:
          - task: twin
            run: "true"
          - task: twin
            run: "true"
  - name: ship
    plan:
      - get: weekly
        passed: [build, "build"]
`

// TestStepsAreBlocksInsideTheirJob: every step is a landing place of its own, however deep, addressed by the keys that reach it, and ends where the next thing at its level begins.
func TestStepsAreBlocksInsideTheirJob(t *testing.T) {
	t.Parallel()

	body := string(highlightConfig(nestedSource, configLinker{}))

	if got := html.UnescapeString(anyTag.ReplaceAllString(body, "")); got != nestedSource {
		t.Fatalf("the wrapped source is not the source:\n%s", got)
	}

	page := "<pre>" + body + "</pre>"

	for id, want := range map[string]string{
		"job-build:ensure":                   "    ensure:\n      task: sweep\n      run: \"true\"",
		"job-build:plan.0":                   "      - get: weekly",
		"job-build:plan.1":                   "      - do:\n          - task: compile\n            run_file: ci/compile.sh\n          - task: lint\n            run_file: ci/absent.sh\n        on_failure:\n          task: report\n          run: \"true\"",
		"job-build:plan.1.do.1":              "          - task: lint\n            run_file: ci/absent.sh",
		"job-build:plan.1.on_failure":        "        on_failure:\n          task: report\n          run: \"true\"",
		"job-build:plan.2.in_parallel.0":     "            - task: unit\n              run: \"true\"",
		"job-build:plan.2.in_parallel.1":     "            - try:\n                task: flaky\n                run: \"true\"",
		"job-build:plan.2.in_parallel.1.try": "            - try:\n                task: flaky\n                run: \"true\"",
		"job-ship:plan.0":                    "      - get: weekly\n        passed: [build, \"build\"]",
	} {
		if got := configBlockText(t, page, id); got != want {
			t.Errorf("block %q is\n%q\nwant\n%q", id, got, want)
		}
	}
}

// TestNamesInTheConfigAreLinks: a name the file declares elsewhere jumps to its entry, an entry with a page links out to it, and an included file's path jumps to its contents below — while a name nothing declares stays text.
func TestNamesInTheConfigAreLinks(t *testing.T) {
	t.Parallel()

	label := func(key string, _ ...any) string { return key }
	links := configLinker{slug: "demo", includes: map[string]string{"ci/compile.sh": "", "prompts/review.md": ""}, label: label}
	body := string(highlightConfig(nestedSource, links))

	if got := html.UnescapeString(anyTag.ReplaceAllString(body, "")); got != nestedSource {
		t.Fatalf("the linked source is not the source:\n%s", got)
	}

	for _, want := range []string{
		`</span> <a class="cfgref" href="#resource-weekly" aria-label="config.ref.def">weekly</a>`,
		`<a class="cfgref" href="#resourcetype-mock" aria-label="config.ref.def">mock</a>`,
		`<a class="cfgref" href="#agent-reviewer" aria-label="config.ref.def">reviewer</a>`,
		`<a class="cfgref" href="#mcp-server-linear" aria-label="config.ref.def">linear</a>`,
		`[</span><a class="cfgref" href="#job-build" aria-label="config.ref.def">build</a>, `,
		`<a class="cfgref" href="#include-ci%2Fcompile.sh" aria-label="config.ref.include">ci/compile.sh</a>`,
		`<a class="cfgref" href="#include-prompts%2Freview.md" aria-label="config.ref.include">prompts/review.md</a>`,
		`<a class="cfgref out" href="/p/demo/jobs/build" aria-label="config.ref.page">build</a>`,
		`<a class="cfgref out" href="/p/demo/resources/weekly" aria-label="config.ref.page">weekly</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the config lacks the link %s:\n%s", want, body)
		}
	}

	if got := strings.Count(body, `href="#resourcetype-mock"`); got != 1 {
		t.Errorf("type: names a resource type under a resource alone; %d are linked", got)
	}

	if got := strings.Count(body, `href="#job-build"`); got != 2 {
		t.Errorf("passed: [build, \"build\"] links %d of its 2 entries", got)
	}

	for _, plain := range []string{">time<", ">nowhere<", ">ci/absent.sh<", ">compile<", `/p/demo/mcp`} {
		if strings.Contains(body, plain) {
			t.Errorf("%s names nothing this file declares or this page shows, and is a link:\n%s", plain, body)
		}
	}

	if strings.Contains(string(highlightConfig(nestedSource, configLinker{})), `class="cfgref out"`) {
		t.Error("a linker with no pipeline cannot link a page")
	}
}

// TestIncludesAreLandingPlaces: the path a step names is the id of the heading its contents sit under.
func TestIncludesAreLandingPlaces(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	const sha = "eeee111122224444"

	err := pipeline.Store.RecordRevision(t.Context(), sha, "jobs:\n  - name: a\n    plan:\n      - task: t\n        run_file: ci/t.sh\n", map[string]string{"ci/t.sh": "echo hi\n"})
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	_, page := get(t, server, "/p/demo/config/"+sha)

	if !strings.Contains(page, `href="#include-ci%2Ft.sh"`) || !strings.Contains(page, `id="include-ci/t.sh"`) {
		t.Errorf("the run_file path does not land on its include:\n%s", page)
	}

	if !strings.Contains(page, `<a class="cfgref out" href="/p/demo/jobs/a" aria-label="a, its page">a</a>`) {
		t.Errorf("the job's name does not link its page, by a name that says so:\n%s", page)
	}
}

// seedDeepRun records nestedSource as a revision and a run of its build job whose rows nest the way a do:, an in_parallel:, a try: and two hooks publish them.
func seedDeepRun(t *testing.T) (*Server, string) {
	t.Helper()

	server, pipeline := testPipeline(t)

	const sha = "eeee111122225555"

	err := pipeline.Store.RecordRevision(t.Context(), sha, nestedSource, nil)
	if err != nil {
		t.Fatalf("RecordRevision: %v", err)
	}

	err = pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "deep", JobName: "build", ConfigSHA: sha})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	started := func(index int, id, parent int64, kind, name string) store.RunEventRow {
		return store.RunEventRow{Type: events.TypeStepStarted, StepIndex: index, StepID: id, ParentStepID: parent, StepKind: kind, StepName: name}
	}

	// As a real run publishes them: the get parents the rest of the plan, whose indexes count from the step after it; a block's children carry the block's index; a hook's row carries -1, the kind "hook", and its key in its name.
	appendEvents(t, pipeline.Store, "deep", []store.RunEventRow{
		started(0, 1, 0, "get", "weekly"),
		started(0, 2, 1, "do", ""),
		started(0, 3, 2, "task", "lint"),
		started(-1, 4, 2, "hook", "on_failure · task report"),
		started(1, 5, 1, "in_parallel", ""),
		started(1, 6, 5, "try", "flaky"),
		started(1, 7, 6, "task", "flaky"),
		started(3, 8, 1, "do", ""),
		started(3, 9, 8, "task", "twin"),
		started(-1, 10, 0, "hook", "ensure · task sweep"),
		started(-1, 11, 0, "hook", "on_abort · task nothing-declares"),
	})

	err = pipeline.Store.FinishRun(t.Context(), "deep", store.RunSucceeded)
	if err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	return server, sha
}

// TestRunStepsJumpToTheirStep: a step's row opens the configuration the run executed AT that step — by position at the top of the plan, by the one step of its kind and name under an already-placed parent below it, and at the parent itself when two could be meant.
func TestRunStepsJumpToTheirStep(t *testing.T) {
	t.Parallel()

	server, sha := seedDeepRun(t)

	_, page := get(t, server, "/p/demo/runs/deep")
	_, config := get(t, server, "/p/demo/config/"+sha)

	heads := regexp.MustCompile(`(?s)<div class="stephead" id="[^"]+_head".*?<span class="headmeta">`).FindAllString(page, -1)
	if len(heads) != 11 {
		t.Fatalf("found %d step heads, want 11:\n%s", len(heads), page)
	}

	stepJump := regexp.MustCompile(`href="/p/demo/config/` + sha + `#([^"]+)"`)
	landed := map[string]int{}

	for _, head := range heads {
		link := stepJump.FindStringSubmatch(head)
		if link == nil {
			landed[""]++

			continue
		}

		fragment, _ := url.PathUnescape(html.UnescapeString(link[1]))
		landed[fragment]++

		configBlockText(t, config, fragment)
	}

	// A try: row is named for the task it wraps, and lands on the try: rather than on that task. plan.4 twice: the do and the twin that could be either. Unlinked: a hook the job does not declare.
	want := map[string]int{"job-build:plan.0": 1, "job-build:plan.1": 1, "job-build:plan.1.do.1": 1, "job-build:plan.1.on_failure": 1, "job-build:plan.2": 1, "job-build:plan.2.in_parallel.1": 1, "job-build:plan.2.in_parallel.1.try": 1, "job-build:plan.4": 2, "job-build:ensure": 1, "": 1}
	if !maps.Equal(landed, want) {
		t.Errorf("rows land on %v, want %v", landed, want)
	}
}

// TestTheLiveStreamCarriesStepConfigLinks: the stream redraws a row with the page's template and its own view of the run, which has to carry the same links or a row loses its link the moment it changes.
func TestTheLiveStreamCarriesStepConfigLinks(t *testing.T) {
	t.Parallel()

	server, sha := seedDeepRun(t)

	if stream := sseHTML(streamOf(t, server, "/p/demo/runs/deep/events")); !strings.Contains(html.UnescapeString(stream), "/p/demo/config/"+sha+"#job-build:plan.1.do.1") {
		t.Errorf("the live stream's rows do not carry their config links:\n%s", stream)
	}
}

// TestConfigLinksShowWithoutAPointer: a phone has no hover, so a link that only appears under one is not there. A name that leads somewhere is underlined at rest (which is also what tells it from the source around it without color), one that leaves the page differently from one that stays, and a step's links to itself and to its config are drawn where nothing can hover.
func TestConfigLinksShowWithoutAPointer(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`(?m)^\.cfgref \{ color: inherit; text-decoration: underline dotted `,
		`(?m)^\.cfgref\.out \{ text-decoration-style: solid; \}`,
		`(?m)^@media \(hover: none\) \{ \.stephead \.anchor \{ opacity: 1; \} \}`,
	} {
		if !regexp.MustCompile(want).Match(css) {
			t.Errorf("app.css has no rule matching %s", want)
		}
	}
}
