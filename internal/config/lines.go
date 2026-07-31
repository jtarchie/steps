package config

// Source line numbers for jobs and plan steps, so a validation error can point
// at a place in the file instead of making the reader count plan entries.

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// stampLines records the source line of every job and plan step by walking the
// document a second time as a raw node tree.
//
// The second parse is deliberate. Giving Step its own UnmarshalYAML to capture
// node.Line would opt the largest key surface in the schema out of
// KnownFields (see strictUnmarshal) — trading typo detection on ~30 keys for a
// line number. Walking the tree afterwards costs one extra parse per load and
// keeps both. Failures here are ignored: the strict decode has already
// succeeded by this point, so a line number is a nicety, never a reason to
// reject a valid pipeline.
func (c *Config) stampLines(data []byte) {
	var doc yaml.Node

	err := yaml.Unmarshal(data, &doc)
	if err != nil || len(doc.Content) == 0 {
		return
	}

	jobs := mappingValue(doc.Content[0], "jobs")
	if jobs == nil {
		return
	}

	for i, jobNode := range jobs.Content {
		if i >= len(c.Jobs) {
			break
		}

		c.Jobs[i].Line = jobNode.Line

		plan := mappingValue(jobNode, "plan")
		if plan == nil {
			continue
		}

		for j, stepNode := range plan.Content {
			if j >= len(c.Jobs[i].Plan) {
				break
			}

			c.Jobs[i].Plan[j].Line = stepNode.Line
		}
	}
}

// mappingValue returns the value node stored under key in a mapping node, or
// nil when the node is not a mapping or has no such key.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}

// at renders the source-location suffix a step's validation label carries —
// ` (line 42)` — or "" when the line is unknown (a step built in Go rather
// than decoded, as tests and any future config synthesis do).
func (s Step) at() string {
	if s.Line <= 0 {
		return ""
	}

	return fmt.Sprintf(" (line %d)", s.Line)
}
