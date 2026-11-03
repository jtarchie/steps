package merkle

import (
	"reflect"
	"testing"
)

// A key yaml.v3 decoded as a non-string spells the same in the cache key as in the call it describes (resource's renderArgValue uses the same config.StringKeys), nested or not.
func TestNormalizeMapKeysSpellsNonStringKeysThroughStringKeys(t *testing.T) {
	t.Parallel()

	got := normalizeMapKeys(map[any]any{
		2024: []any{map[any]any{true: "yes"}},
		"on": map[any]any{1.5: "x"},
	})

	want := map[string]any{
		"2024": []any{map[string]any{"true": "yes"}},
		"on":   map[string]any{"1.5": "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeMapKeys = %#v, want %#v", got, want)
	}
}
