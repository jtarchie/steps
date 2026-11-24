package web

import (
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

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

	body := string(highlightConfig(source))

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

	if got := html.UnescapeString(anyTag.ReplaceAllString(string(highlightConfig(source)), "")); got != source {
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
