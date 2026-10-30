package config

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// readKeyPattern is a source. or params. read in an embedded type's expr (source.limit) or template ({{ .source.uri }}, index .source "fetch").
var readKeyPattern = regexp.MustCompile(`\b(source|params)(?:\.([a-z_]+)| "([a-z_]+)")`)

// An embedded type reads its source: and params: in text the loader never parses, so nothing else ties those reads to the closed shape that refuses every other key: a key read but not declared is refused at load, and one declared but not read loads and does nothing.
func TestNativeYAMLTypesReadExactlyTheirShape(t *testing.T) {
	t.Parallel()

	shapes := map[string]struct{ source, params any }{
		gitType:           {source: GitSource{}},
		slackMentionsType: {source: SlackMentionsSource{}},
		slackReplyType:    {source: SlackReplySource{}, params: SlackReplyParams{}},
		slackReactionType: {source: SlackReactionSource{}, params: SlackReactionParams{}},
	}

	names, err := ListBuiltinResourceTypeNames()
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range names {
		shape, ok := shapes[name]
		if !ok {
			t.Errorf("built-in type %q has no shape: give it a source: struct and add it here", name)

			continue
		}

		data, err := builtinFS.ReadFile("resource_types/" + name + ".yml")
		if err != nil {
			t.Fatal(err)
		}

		read := keysRead(string(data))

		for section, declared := range map[string]any{"source": shape.source, "params": shape.params} {
			var want []string
			if declared != nil {
				want = slices.Sorted(slices.Values(yamlKeys(declared)))
			}

			if !slices.Equal(read[section], want) {
				t.Errorf("%s reads %s keys %v, but its shape declares %v", name, section, read[section], want)
			}
		}
	}
}

// keysRead is every source. and params. key text reads outside its comments, by section, sorted.
func keysRead(text string) map[string][]string {
	read := map[string][]string{}

	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "//") {
			continue
		}

		for _, match := range readKeyPattern.FindAllStringSubmatch(line, -1) {
			read[match[1]] = append(read[match[1]], match[2]+match[3])
		}
	}

	for section, keys := range read {
		read[section] = slices.Compact(slices.Sorted(slices.Values(keys)))
	}

	return read
}
