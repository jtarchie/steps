package web

// What the /docs converter promises: the anchors the pages link each other by, and the highlighting they are read with.

import (
	"strings"
	"testing"
)

// renderDocs is what handleDocs itself calls, over source a test chooses rather than an embedded page.
func renderDocs(t *testing.T, source string) string {
	t.Helper()

	html, err := renderDocsMarkdown([]byte(source))
	if err != nil {
		t.Fatalf("rendering docs markdown: %v", err)
	}

	return html
}

// TestDocHeadingsKeepTheirUnderscores: the pages cross-link each other by hand-written anchors, and a field name is the most common thing they point at — goldmark's own generator folds `_` into `-`, so every `#max_visits` link in the corpus resolves only while docs.Slug is the generator actually in use.
func TestDocHeadingsKeepTheirUnderscores(t *testing.T) {
	t.Parallel()

	html := renderDocs(t, "## `max_visits` and friends\n\ntext\n")

	if !strings.Contains(html, `id="max_visits-and-friends"`) {
		t.Errorf("the heading id is not the one docs.Slug spells:\n%s", html)
	}
}

// TestRepeatedDocHeadingsDedupePerPage: two sections can legitimately share a name, and two elements cannot share an id — the numbering is goldmark's own now rather than this package's map, so the thing worth asserting is that a repeat still gets one.
func TestRepeatedDocHeadingsDedupePerPage(t *testing.T) {
	t.Parallel()

	html := renderDocs(t, "## Caching\n\none\n\n## Caching\n\ntwo\n")

	if !strings.Contains(html, `id="caching"`) || !strings.Contains(html, `id="caching-1"`) {
		t.Errorf("a repeated heading did not get its own id:\n%s", html)
	}
}

// TestDocHeadingIDsDoNotLeakBetweenRenders: the set of ids already taken rides on the parse context, so a context reused across requests would number the second reader's first heading as a duplicate of the first reader's.
func TestDocHeadingIDsDoNotLeakBetweenRenders(t *testing.T) {
	t.Parallel()

	const page = "## Caching\n\none\n"

	first := renderDocs(t, page)
	second := renderDocs(t, page)

	if first != second {
		t.Errorf("two renders of one page differ:\n%s\n---\n%s", first, second)
	}

	if strings.Contains(second, `id="caching-1"`) {
		t.Errorf("the second render inherited the first render's ids:\n%s", second)
	}
}

// TestDocFencedCodeIsHighlightedInTheUIsOwnStyle: the highlighter is an extension of both halves of the converter now, and a page whose fences render as plain text is what a half-wired one looks like — the inline color is the assertion, because docsCodeStyle exists precisely so the stock themes are not used.
func TestDocFencedCodeIsHighlightedInTheUIsOwnStyle(t *testing.T) {
	t.Parallel()

	html := renderDocs(t, "```yaml\njobs:\n  - name: build\n```\n")

	if !strings.Contains(html, "<span") {
		t.Errorf("the fenced block was not tokenised at all:\n%s", html)
	}

	// --blue, the color docsCodeStyle gives a YAML key; a stock chroma theme paints it red.
	if !strings.Contains(strings.ToLower(html), "#7aa4d9") {
		t.Errorf("the block was highlighted in something other than the UI's own palette:\n%s", html)
	}
}
