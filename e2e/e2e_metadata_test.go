package e2e

// steps#272 parts 1 and 2: a get's and a put's metadata — "facts" — are shown
// under the step, listed as what the run published, kept beside the version,
// and read back when a fetch is reused rather than repeated.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/sqlite"
)

// factsPipeline answers in Concourse's shape from both stages: the get's in:
// prints its version beside its metadata, and the put's out: does the same,
// which is the upgrade case — that object used to be read as the version.
const factsPipeline = `
resource_types:
- name: ticket
  config:
    check: |
      printf '[{"id": "412"}]'
    in: |
      echo fetching >&2
      printf '{"version": {"id": "412"}, "metadata": [{"name": "url", "value": "https://tickets.example/412"}, {"name": "title", "value": "Add the widget cache"}]}'
    out: |
      printf '{"version": {"id": "2291"}, "metadata": [{"name": "url", "value": "https://tickets.example/412#c2291"}, {"name": "note", "value": "javascript:alert(1)"}]}'
resources:
- name: pr
  type: ticket
  source: {}
- name: review
  type: ticket
  source: {}
jobs:
- name: answer
  plan:
  - get: pr
    trigger: true
  - put: review
`

func TestEndToEndFactsAreShownPublishedAndKeptWithTheVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fetches := filepath.Join(dir, "fetches")
	path := writePipeline(t, dir, strings.Replace(factsPipeline, "echo fetching >&2", "echo fetched >> "+fetches, 1))
	root := filepath.Join(dir, "root")

	screen := ansi.ReplaceAllString(runCaptured(t, "run", path, "--job", "answer", "--progress", "tty", "--workspace-root", root), "")

	assertContainsAll(t, "the live view", screen,
		"    url:   https://tickets.example/412\n",
		"    title: Add the widget cache\n",
		"    url:  https://tickets.example/412#c2291\n",
		"Published\n  review  https://tickets.example/412#c2291\n",
	)

	if strings.Contains(screen, "Published\n  review  javascript:") || strings.Contains(screen, "  pr  https://tickets.example/412\n") {
		t.Errorf("Published lists only a put's http(s) links:\n%s", screen)
	}

	st, err := sqlite.OpenExisting(string(cli.StatePath(path, "")), cli.PipelineName(path))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = st.Close() })

	assertKept(t, st, "review", `{"id":"2291"}`, "https://tickets.example/412#c2291")
	assertKept(t, st, "pr", `{"id":"412"}`, "https://tickets.example/412")

	// The same root again: the resource cache hands the get its earlier fetch, so no in: runs, and the facts come from what was kept.
	again := runCaptured(t, "run", path, "--job", "answer", "--progress", "plain", "--workspace-root", root)

	if log, _ := os.ReadFile(fetches); strings.Count(string(log), "fetched") != 1 { //nolint:gosec // a t.TempDir() file this test named
		t.Fatalf("in: ran %d times over two runs, want once — the second run was to reuse the first's fetch", strings.Count(string(log), "fetched"))
	}

	assertContainsAll(t, "plain output", again, "  url: https://tickets.example/412\n", "  title: Add the widget cache\n", "  url: https://tickets.example/412#c2291\n")

	if strings.Contains(again, "Published") {
		t.Errorf("plain output has no end-of-run summary, so no Published list:\n%s", again)
	}
}

// runCaptured is one steps invocation that must succeed, and what it printed.
func runCaptured(t *testing.T, args ...string) string {
	t.Helper()

	var runErr error

	out := captureStdout(t, func() { runErr = stepsRun(t, args) })
	if runErr != nil {
		t.Fatalf("steps %v: %v\n%s", args, runErr, out)
	}

	return out
}

func assertContainsAll(t *testing.T, what, text string, wants ...string) {
	t.Helper()

	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s is missing %q:\n%s", what, want, text)
		}
	}
}

// TestEndToEndTheGitHubTypesReportTheirLinks: with no pipeline change, a github-prs get reports the pull request's link, title and author from the read it already makes, and a github-pr-comment put the new comment's link.
func TestEndToEndTheGitHubTypesReportTheirLinks(t *testing.T) {
	t.Parallel()

	fake := newFakeGitHub(t)
	fake.addPR(reviewablePR())

	path := writePipeline(t, t.TempDir(), githubPipeline(fake, ""))

	screen := ansi.ReplaceAllString(runCaptured(t, "run", path, "--job", "review", "--progress", "tty"), "")
	pull := "https://github.example/acme/app/pull/7"

	for _, want := range []string{
		"    url:    " + pull + "\n",
		"    title:  Add a greeting\n",
		"    author: alice\n",
		"Published\n  comment  " + pull + "#issuecomment-",
	} {
		if !strings.Contains(screen, want) {
			t.Errorf("the run is missing %q:\n%s", want, screen)
		}
	}
}

func assertKept(t *testing.T, st store.Versions, resource, version, want string) {
	t.Helper()

	kept, err := st.VersionMetadata(t.Context(), resource, []store.VersionKey{store.VersionKey(version)})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(kept[store.VersionKey(version)], want) {
		t.Errorf("%s %s keeps %q, want it to hold %q", resource, version, kept, want)
	}
}

// TestEndToEndTheRunJobAndResourcePagesShowFacts: the run page draws each step's facts and lists what the run published; the resource and job pages draw them beside the version. A value that is not an http(s) address is text, never a link.
func TestEndToEndTheRunJobAndResourcePagesShowFacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := writePipeline(t, dir, strings.Replace(factsPipeline,
		`"title", "value": "Add the widget cache"`, `"title", "value": "<script>alert(1)</script>"`, 1))
	name := cli.PipelineName(path)

	served := startWebFor(t, path, "--interval", "1h")
	waitForQueueSuccess(t, served.state, name, 1)

	run := newestRun(t, served.state, name, "answer")

	pages := map[string]string{}
	for _, page := range []string{"/p/" + name + "/runs/" + run.ID, "/p/" + name + "/resources/pr", "/p/" + name + "/jobs/answer"} {
		code, body := served.get(t, page)
		if code != 200 {
			t.Fatalf("GET %s = %d", page, code)
		}

		pages[page] = body
	}

	runPage := pages["/p/"+name+"/runs/"+run.ID]

	for _, want := range []string{
		`<dt>url</dt><dd><a href="https://tickets.example/412" rel="nofollow noopener noreferrer">https://tickets.example/412</a></dd>`,
		`<dt>note</dt><dd>javascript:alert(1)</dd>`,
		`<dt>title</dt><dd>&lt;script&gt;alert(1)&lt;/script&gt;</dd>`,
		`<span class="pubstep">review</span> <a href="https://tickets.example/412#c2291"`,
	} {
		if !strings.Contains(runPage, want) {
			t.Errorf("the run page is missing %s", want)
		}
	}

	if strings.Contains(runPage, `<span class="pubstep">pr</span>`) {
		t.Error("the run page lists a get's link as published")
	}

	for _, page := range []string{"/p/" + name + "/resources/pr", "/p/" + name + "/jobs/answer"} {
		if !strings.Contains(pages[page], `<a href="https://tickets.example/412" rel="nofollow noopener noreferrer">`) {
			t.Errorf("%s does not show pr's url beside its version", page)
		}
	}

	served.stop(t)

	_ = os.Remove(served.state)
}
