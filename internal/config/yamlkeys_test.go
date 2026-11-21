package config

import (
	"strings"
	"testing"
)

// yaml.v4 decodes a mapping with a non-string key (an unquoted on:, a year) into map[any]any; both the cache key and the call it describes must spell those keys the same way, so they share this one conversion.
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

func TestKeysStringKeysWouldSpellAlikeAreRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, source, want string }{
		{"a float and a string", "a:\n  b:\n    \"1\": x\n    1.0: y\n", `line 4: key 1.0 and key "1" on line 3 are both "1"`},
		{"an int and a float", "a: {1: x, 1.0: y}\n", `line 1: key 1.0 and key 1 on line 1 are both "1"`},
		{"two spellings of null", "a: {~: x, null: y}\n", `line 1: key null and key ~ on line 1 are both "<nil>"`},
		{"inside a list", "a:\n- {2: x, 2.0: y}\n", `key 2.0 and key 2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := refuseCollidingKeys([]byte(tc.source))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refuseCollidingKeys = %v, want an error containing %q", err, tc.want)
			}
		})
	}

	for _, source := range []string{"a: {1: x, 2: y, \"3\": z, 1.5: w}\n", "a: &x {k: 1}\nb:\n  <<: *x\n  k: 2\n", "not: [valid\n"} {
		err := refuseCollidingKeys([]byte(source))
		if err != nil {
			t.Errorf("refuseCollidingKeys(%q) = %v, want nil", source, err)
		}
	}
}
