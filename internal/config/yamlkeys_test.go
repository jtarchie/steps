package config

import "testing"

// yaml.v3 decodes a mapping with a non-string key (an unquoted on:, a year) into map[any]any; both the cache key and the call it describes must spell those keys the same way, so they share this one conversion.
func TestStringKeysSpellsEveryKeyTheWayFmtSprintDoes(t *testing.T) {
	t.Parallel()

	got := StringKeys(map[any]any{"a": 1, 2: "two", true: "yes", 2.5: "frac", nil: "nil"})

	want := map[string]any{"a": 1, "2": "two", "true": "yes", "2.5": "frac", "<nil>": "nil"}
	if len(got) != len(want) {
		t.Fatalf("StringKeys = %v, want %v", got, want)
	}

	for key, value := range want {
		if got[key] != value {
			t.Errorf("StringKeys[%q] = %v, want %v", key, got[key], value)
		}
	}
}

func TestStringKeysLeavesValuesAlone(t *testing.T) {
	t.Parallel()

	inner := map[any]any{1: "x"}
	got := StringKeys(map[any]any{"k": inner})

	if _, still := got["k"].(map[any]any); !still {
		t.Errorf("StringKeys rewrote a nested value: %T", got["k"])
	}
}
