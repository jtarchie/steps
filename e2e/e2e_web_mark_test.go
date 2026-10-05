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
