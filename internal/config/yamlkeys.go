package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// StringKeys gives a mapping yaml.v3 decoded as map[any]any (an unquoted on:, a filter keyed by a year) string keys, one level deep and leaving values as they are. The cache key (merkle) and the call it describes (resource) both convert through here, because they must agree on how a key is spelled.
func StringKeys(mapping map[any]any) map[string]any {
	keyed := make(map[string]any, len(mapping))
	for key, value := range mapping {
		keyed[stringKey(key)] = value
	}

	return keyed
}

func stringKey(key any) string {
	return fmt.Sprint(key)
}

// refuseCollidingKeys refuses a mapping two of whose keys StringKeys spells alike (1.0 and "1"): yaml.v3 keeps both, and StringKeys would keep one at map-iteration random, separately for the call and its cache key.
func refuseCollidingKeys(source []byte) error {
	return collidingKeys(parseNode(source))
}

// parseNode is source's document node, or an empty one when source does not parse: the strict decode reports a malformed document with its line, and every walk over the node tree finds nothing in an empty one.
func parseNode(source []byte) *yaml.Node {
	var doc yaml.Node

	err := yaml.Unmarshal(source, &doc)
	if err != nil {
		return &yaml.Node{}
	}

	return &doc
}

func collidingKeys(node *yaml.Node) error {
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]*yaml.Node, len(node.Content)/2)

		for i := 0; i+1 < len(node.Content); i += 2 {
			keyNode := node.Content[i]

			var key any

			err := keyNode.Decode(&key)
			if err != nil {
				continue
			}

			spelled := stringKey(key)
			if first, ok := seen[spelled]; ok {
				return fmt.Errorf("line %d: key %s and key %s on line %d are both %q once read, so one would silently replace the other", keyNode.Line, written(keyNode), written(first), first.Line, spelled)
			}

			seen[spelled] = keyNode
		}
	}

	for _, child := range node.Content {
		err := collidingKeys(child)
		if err != nil {
			return err
		}
	}

	return nil
}

// written is a key as the author typed it, quotes included, since the quotes are the difference being reported.
func written(node *yaml.Node) string {
	if node.Style&(yaml.DoubleQuotedStyle|yaml.SingleQuotedStyle) != 0 {
		return fmt.Sprintf("%q", node.Value)
	}

	return node.Value
}
