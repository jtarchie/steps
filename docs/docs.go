// Package docs embeds the user-facing documentation (docs/*.md) and
// extracts its fenced ```yaml blocks, which are the repo's tested example
// corpus: docs_test.go (root package) schema-validates and executes them, so
// a doc example cannot drift from what the runner actually accepts.
//
// The fence info string is a tiny grammar deciding how a block is tested:
//
//	```yaml                a complete pipeline; schema-validated and executed
//	```yaml test=<id>      executed against the fake LLM scenario named <id>
//	                       (docs_scenarios_test.go) — required for agent steps
//	```yaml noexec         schema-validated and loaded, not executed (needs
//	                       docker, the network, or real credentials)
//	```yaml fragment       rendered only; not a complete pipeline
//
// proposals/ is deliberately outside the embed: sketches are not promises.
package docs

import (
	"embed"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

//go:embed *.md
var content embed.FS

// Block is one fenced ```yaml example lifted from a doc page.
type Block struct {
	Page string // file name, e.g. "agents.md"
	Line int    // 1-based line number of the opening fence
	Info string // fence info string after "yaml", e.g. "test=verdicts"
	Body string
}

// Mode reports how the block is tested: "run", "noexec", or "fragment".
func (b Block) Mode() string {
	for _, field := range strings.Fields(b.Info) {
		switch {
		case field == "noexec" || field == "fragment":
			return field
		case strings.HasPrefix(field, "test="):
			return "run"
		}
	}

	return "run"
}

// TestID is the fake-LLM scenario this block runs against, "" when none.
func (b Block) TestID() string {
	for _, field := range strings.Fields(b.Info) {
		if id, ok := strings.CutPrefix(field, "test="); ok {
			return id
		}
	}

	return ""
}

// Name identifies the block in test output: page, line, and scenario if any.
func (b Block) Name() string {
	if id := b.TestID(); id != "" {
		return fmt.Sprintf("%s:%d(%s)", b.Page, b.Line, id)
	}

	return fmt.Sprintf("%s:%d", b.Page, b.Line)
}

// Pages lists the embedded doc pages, README.md first (it is the index),
// then alphabetically.
func Pages() []string {
	entries, err := content.ReadDir(".")
	if err != nil {
		// The embed is compiled in; an unreadable root is impossible.
		return nil
	}

	names := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.Name() != "README.md" {
			names = append(names, entry.Name())
		}
	}

	sort.Strings(names)

	return append([]string{"README.md"}, names...)
}

// Page returns one page's markdown source.
func Page(name string) ([]byte, error) {
	body, err := content.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("no doc page %q", name)
	}

	return body, nil
}

// Group is a curated section of the corpus, in reading order — the same
// order docs/README.md's own tables teach. Pages() stays alphabetical for
// enumeration; Groups() is what a navigation surface should render, so the
// rail agrees with the curriculum instead of sorting agents-internals ahead
// of agents.
type Group struct {
	Title string
	Pages []string
}

// Groups returns every page but the index, grouped and ordered for reading.
// TestDocsGroupsComplete (root package) keeps this table complete: a new
// page must be placed in a group, or the build is red.
func Groups() []Group {
	return []Group{
		{Title: "Writing pipelines", Pages: []string{
			"resources.md", "control-flow.md", "agents.md",
			"attempts-timeout.md", "workspace.md", "infra.md",
			"templating.md", "mcp.md", "complete.md",
		}},
		{Title: "Reference", Pages: []string{
			"web.md", "agents-internals.md", "conformance.md",
		}},
	}
}

// Slug converts a heading's text to its GitHub-style anchor id: lowercased,
// punctuation dropped, spaces hyphenated, underscores and hyphens kept.
//
// This is the algorithm the corpus's hand-written #fragment links were
// authored against (the pages are read on GitHub too), so the web renderer
// generates ids with THIS rather than goldmark's default — which folds "_"
// into "-" and silently strands every anchor containing a field name like
// max_visits. TestDocsAnchors holds the two in agreement.
func Slug(heading string) string {
	var out strings.Builder

	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			out.WriteByte('-')
		case r == '-' || r == '_' ||
			unicode.IsLetter(r) || unicode.IsDigit(r):
			out.WriteRune(r)
		}
	}

	return out.String()
}

// Heading is one heading on a page, with the anchor id it renders under.
type Heading struct {
	Level int
	Text  string
	ID    string
}

// Headings returns every heading on a page in order, each with its
// GitHub-style id, deduplicated the way GitHub does (repeats get -1, -2,
// ...). Fenced code blocks are skipped — a "# comment" inside one is not a
// heading. This is the id set the web renderer emits and the anchor test
// verifies against, so all three surfaces agree by construction.
func Headings(body string) []Heading {
	var (
		headings []Heading
		seen     = map[string]int{}
		fence    bool
	)

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "```") {
			fence = !fence

			continue
		}

		marks := len(line) - len(strings.TrimLeft(line, "#"))
		if fence || marks == 0 || marks > 6 || !strings.HasPrefix(line[marks:], " ") {
			continue
		}

		text := stripMarkdown(strings.TrimSpace(line[marks:]))
		slug := Slug(text)

		if n, dup := seen[slug]; dup {
			seen[slug] = n + 1
			slug = fmt.Sprintf("%s-%d", slug, n)
		} else {
			seen[slug] = 1
		}

		headings = append(headings, Heading{Level: marks, Text: text, ID: slug})
	}

	return headings
}

// stripMarkdown removes the inline markers a heading may carry (`code`,
// **bold**, *em*) so slugging sees the text GitHub slugs.
func stripMarkdown(text string) string {
	return strings.NewReplacer("`", "", "*", "").Replace(text)
}

// Blocks extracts every ```yaml block from every page. An unterminated
// fence is an authoring error and reported as one, not silently swallowed.
func Blocks() ([]Block, error) {
	var blocks []Block

	for _, page := range Pages() {
		body, err := Page(page)
		if err != nil {
			return nil, err
		}

		pageBlocks, err := extract(page, string(body))
		if err != nil {
			return nil, err
		}

		blocks = append(blocks, pageBlocks...)
	}

	return blocks, nil
}

// extract scans one page for fenced code blocks, keeping the yaml ones.
// Non-yaml fences are still tracked so their contents (which may themselves
// contain ``` lines rendered as text) never open a phantom block.
func extract(page, body string) ([]Block, error) {
	var (
		blocks  []Block
		current *Block
		isYAML  bool
		inside  bool
		lines   []string
	)

	for i, line := range strings.Split(body, "\n") {
		fence := strings.HasPrefix(line, "```")

		switch {
		case fence && !inside:
			inside = true
			info := strings.TrimSpace(strings.TrimPrefix(line, "```"))
			lang, rest, _ := strings.Cut(info, " ")
			isYAML = lang == "yaml"
			current = &Block{Page: page, Line: i + 1, Info: strings.TrimSpace(rest)}

			lines = lines[:0]
		case fence:
			if isYAML {
				current.Body = strings.Join(lines, "\n") + "\n"
				blocks = append(blocks, *current)
			}

			inside = false
		case inside:
			lines = append(lines, line)
		}
	}

	if inside {
		return nil, fmt.Errorf("%s:%d: unterminated code fence", page, current.Line)
	}

	return blocks, nil
}
