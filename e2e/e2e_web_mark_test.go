package e2e

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/cli"
)

// TestAPipelinesTabSaysWhetherItIsRed is the glance-status promise from the
// tab strip: a pinned pipeline tab used to wear the same grey dot whatever
// its jobs did, so finding out meant opening it. Its title and icon now carry
// the pipeline's mark — red while any job's latest finished run failed, and
// green again once that job passes, with nothing else touched.
func TestAPipelinesTabSaysWhetherItIsRed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demo.yml")
	gate := filepath.Join(dir, "fixed")

	writePipelineFile(t, path, `
jobs:
- name: fine
  plan:
  - task: work
    run: "true"
- name: flaky
  plan:
  - task: work
    run: test -e `+gate+`
`)

	mustRun(t, "run", path, "--job", "fine")

	err := cli.Run([]string{"run", path, "--job", "flaky"})
	if err == nil {
		t.Fatal("the gated job ran green before its gate existed")
	}

	server, _ := webServerFor(t, path)

	_, board := webGet(t, server, "/p/demo")
	if title := titleOf(t, board); !strings.HasPrefix(title, "✗ ") {
		t.Errorf("a pipeline with a failed job is titled %q, want it to lead with ✗", title)
	}

	if icon := faviconOf(t, board); !strings.Contains(icon, "%23e0645a") {
		t.Errorf("a pipeline with a failed job wears %q, want the red disc", icon)
	}

	err = os.WriteFile(gate, nil, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	mustRun(t, "run", path, "--job", "flaky")

	_, board = webGet(t, server, "/p/demo")
	if title := titleOf(t, board); !strings.HasPrefix(title, "✓ ") {
		t.Errorf("a pipeline whose jobs all passed is titled %q, want it to lead with ✓", title)
	}

	if icon := faviconOf(t, board); !strings.Contains(icon, "%2384c06d") {
		t.Errorf("a pipeline whose jobs all passed wears %q, want the green disc", icon)
	}
}

var (
	titleTag   = regexp.MustCompile(`<title>([^<]*)</title>`)
	faviconTag = regexp.MustCompile(`<link rel="icon" id="favicon" href="([^"]*)"`)
)

func titleOf(t *testing.T, page string) string {
	t.Helper()

	match := titleTag.FindStringSubmatch(page)
	if match == nil {
		t.Fatal("the page has no <title>")
	}

	return match[1]
}

func faviconOf(t *testing.T, page string) string {
	t.Helper()

	match := faviconTag.FindStringSubmatch(page)
	if match == nil {
		t.Fatal("the page has no favicon link")
	}

	return match[1]
}

// TestTheSwitcherSaysWhichPipelineIsRed: the switcher listed names and job
// counts, so learning which of a daemon's pipelines was broken took opening
// each. Every row now leads with its pipeline's mark — read here from the
// OTHER pipeline's page, which is where the reader who has not looked is.
func TestTheSwitcherSaysWhichPipelineIsRed(t *testing.T) {
	dir := t.TempDir()
	green := filepath.Join(dir, "green.yml")
	red := filepath.Join(dir, "red.yml")

	writePipelineFile(t, green, `
jobs:
- name: ok
  plan:
  - task: work
    run: "true"
`)
	writePipelineFile(t, red, `
jobs:
- name: broken
  plan:
  - task: work
    run: exit 1
`)

	mustRun(t, "run", green, "--job", "ok")

	err := cli.Run([]string{"run", red, "--job", "broken"})
	if err == nil {
		t.Fatal("the broken job ran green")
	}

	server := webServerForAll(t, green, red)

	_, page := webGet(t, server, "/p/green")

	for slug, word := range map[string]string{"green": "passed", "red": "failed"} {
		row := switcherRow(t, page, slug)
		if !strings.Contains(row, `class="st-`+word+`"`) || !strings.Contains(row, `<span class="visually-hidden">`+word) {
			t.Errorf("the %s pipeline's switcher row does not say %s:\n%s", slug, word, row)
		}
	}
}

// switcherRow is one pipeline's option in the switcher menu.
func switcherRow(t *testing.T, page, slug string) string {
	t.Helper()

	start := strings.Index(page, `role="option"`)
	for start >= 0 {
		end := strings.Index(page[start:], "</a>")
		if end < 0 {
			break
		}

		row := page[start : start+end]
		if strings.Contains(row, `href="/p/`+slug+`"`) {
			return row
		}

		next := strings.Index(page[start+end:], `role="option"`)
		if next < 0 {
			break
		}

		start += end + next
	}

	t.Fatalf("no switcher row for %s", slug)

	return ""
}
