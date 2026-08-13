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
		return nil, fmt.Errorf("no doc page %q (see Pages)", name)
	}

	return body, nil
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
