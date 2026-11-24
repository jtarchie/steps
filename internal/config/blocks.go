package config

import (
	"strings"

	"go.yaml.in/yaml/v4"
)

// SourceBlock is where one named entry of a top-level list sits in a pipeline's source text, by 1-based inclusive line.
type SourceBlock struct {
	// Section is the top-level key the entry is listed under: "jobs", "resources".
	Section string
	Name    string
	Start   int
	End     int
}

// SourceBlocks locates every named entry of every top-level list in source, in file order. A source that will not parse has none, and neither does an entry sharing its first line with another (a flow list): a block is whole lines.
func SourceBlocks(source string) []SourceBlock {
	var doc yaml.Node

	err := yaml.Unmarshal([]byte(source), &doc)
	if err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	lines := strings.Split(source, "\n")
	root := doc.Content[0].Content

	var blocks []SourceBlock

	for i := 0; i+1 < len(root); i += 2 {
		key, list := root[i], root[i+1]
		if list.Kind != yaml.SequenceNode {
			continue
		}

		sectionEnd := len(lines)
		if i+2 < len(root) {
			sectionEnd = root[i+2].Line - 1
		}

		blocks = append(blocks, sectionBlocks(lines, key, list, sectionEnd)...)
	}

	return blocks
}

func sectionBlocks(lines []string, key, list *yaml.Node, sectionEnd int) []SourceBlock {
	var blocks []SourceBlock

	last := key.Line

	for j, item := range list.Content {
		name := entryName(item)
		if name == "" || item.Line <= last {
			continue
		}

		last = item.Line

		end := sectionEnd
		if j+1 < len(list.Content) {
			end = list.Content[j+1].Line - 1
		}

		end = trimTrailing(lines, item.Line, end, list.Column-1)
		blocks = append(blocks, SourceBlock{Section: key.Value, Name: name, Start: item.Line, End: end})
	}

	return blocks
}

func entryName(item *yaml.Node) string {
	if item.Kind != yaml.MappingNode {
		return ""
	}

	for i := 0; i+1 < len(item.Content); i += 2 {
		if item.Content[i].Value == "name" && item.Content[i+1].Kind == yaml.ScalarNode {
			return item.Content[i+1].Value
		}
	}

	return ""
}

// trimTrailing pulls end back over blank lines and over comments indented no deeper than the list's dash: those introduce whatever follows, and nothing inside the entry (a block scalar's text included) can sit that far left.
func trimTrailing(lines []string, start, end, dash int) int {
	for end > start && end <= len(lines) {
		text := strings.TrimLeft(lines[end-1], " ")
		indent := len(lines[end-1]) - len(text)

		if text != "" && (!strings.HasPrefix(text, "#") || indent > dash) {
			break
		}

		end--
	}

	return end
}
