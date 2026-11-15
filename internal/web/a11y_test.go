package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/docs"
)

// TestEveryPageOffersASkipToItsContent: the status bar and the action bar sit before every page's content, so a keyboard reader tabbed through a dozen controls to reach it (WCAG 2.4.1). The skip link must be the FIRST thing focusable, its target must take focus, and the bar must be a <header> landmark so a screen reader's landmark list names it rather than leaving it loose outside every region.
func TestEveryPageOffersASkipToItsContent(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, path := range []string{"/p/demo", "/p/demo/runs", "/p/demo/resources", "/p/demo/approvals", "/p/demo/questions", "/docs/README.md", "/nosuch"} {
		_, page := get(t, server, path)

		_, body, _ := strings.Cut(page, "<body")
		first := regexp.MustCompile(`<(a|button|input|select|textarea)\b[^>]*>`).FindString(body)

		if !strings.Contains(first, `href="#main"`) {
			t.Errorf("%s: the first focusable element is %s, not a skip to #main", path, first)
		}

		if !strings.Contains(page, `<main id="main" tabindex="-1">`) {
			t.Errorf("%s: <main> is not a skip target that can take focus", path)
		}

		header, bar, headerEnd, main := strings.Index(page, "<header"), strings.Index(page, `id="statusline"`), strings.Index(page, "</header>"), strings.Index(page, "<main")
		if header < 0 || header > bar || headerEnd < bar || headerEnd > main {
			t.Errorf("%s: the status bar is not inside a <header> landmark before <main>", path)
		}
	}
}

// TestDocsTablesNameEveryColumn: an empty header cell is a column a screen reader announces as nothing while reading each row.
func TestDocsTablesNameEveryColumn(t *testing.T) {
	t.Parallel()

	for _, name := range docs.Pages() {
		body, err := docs.Page(name)
		if err != nil {
			t.Fatal(err)
		}

		out, err := renderDocsMarkdown(body)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(out, "<th></th>") {
			t.Errorf("%s: a table has an empty header cell", name)
		}
	}
}
