package config

import "fmt"

// StringKeys gives a mapping yaml.v3 decoded as map[any]any (an unquoted on:, a filter keyed by a year) string keys, one level deep and leaving values as they are. The cache key (merkle) and the call it describes (resource) both convert through here, because they must agree on how a key is spelled.
func StringKeys(mapping map[any]any) map[string]any {
	keyed := make(map[string]any, len(mapping))
	for key, value := range mapping {
		keyed[fmt.Sprint(key)] = value
	}

	return keyed
}
