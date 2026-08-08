package pipeline

// Decoding a from: axis's recorded array (#42): the shapes accepted, and the
// ones refused with a message naming what was found.

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func TestDecodeAxisValuesAcceptsStringsAndObjects(t *testing.T) {
	t.Parallel()

	axis := config.AcrossVar{Var: "finding", From: "findings"}

	t.Run("strings", func(t *testing.T) {
		t.Parallel()

		values, err := decodeAxisValues(axis, map[string]string{"findings": `["alpha","beta"]`})
		if err != nil {
			t.Fatalf("decodeAxisValues: %v", err)
		}

		want := []any{"alpha", "beta"}
		if len(values) != len(want) {
			t.Fatalf("values = %v, want %v", values, want)
		}

		for i := range want {
			if values[i] != want[i] {
				t.Errorf("value %d = %v, want %v", i, values[i], want[i])
			}
		}
	})

	t.Run("objects with scalar fields", func(t *testing.T) {
		t.Parallel()

		values, err := decodeAxisValues(axis, map[string]string{
			"findings": `[{"id":"A-1","line":42,"blocking":true,"ratio":1.50}]`,
		})
		if err != nil {
			t.Fatalf("decodeAxisValues: %v", err)
		}

		got, ok := values[0].(map[string]string)
		if !ok {
			t.Fatalf("value = %T, want map[string]string", values[0])
		}

		// Numbers keep their own text: a line number must render "42", not
		// float64's formatting of it, and a decimal must not be re-rounded.
		for key, want := range map[string]string{"id": "A-1", "line": "42", "blocking": "true", "ratio": "1.50"} {
			if got[key] != want {
				t.Errorf("field %q = %q, want %q", key, got[key], want)
			}
		}
	})
}

func TestDecodeAxisValuesRefusesShapesWithNoRendering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, recorded, wantErr string
	}{
		{
			name:     "mixed strings and objects",
			recorded: `["alpha",{"id":"A-1"}]`,
			wantErr:  "mixes shapes",
		},
		{
			name:     "nested object inside an item",
			recorded: `[{"id":"A-1","where":{"file":"a.py"}}]`,
			wantErr:  "nested object",
		},
		{
			name:     "list inside an item",
			recorded: `[{"id":"A-1","files":["a.py","b.py"]}]`,
			wantErr:  "list",
		},
		{
			name:     "null field",
			recorded: `[{"id":"A-1","line":null}]`,
			wantErr:  "null",
		},
		{
			name:     "empty object",
			recorded: `[{}]`,
			wantErr:  "empty object",
		},
		{
			name:     "not an array",
			recorded: `{"id":"A-1"}`,
			wantErr:  "must hold a JSON array",
		},
		{
			name:     "array of numbers",
			recorded: `[1,2]`,
			wantErr:  "neither a string nor an object",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := decodeAxisValues(
				config.AcrossVar{Var: "finding", From: "findings"},
				map[string]string{"findings": tc.recorded},
			)
			if err == nil {
				t.Fatalf("%s decoded", tc.name)
			}

			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}

			// Every message names the key, because the author is debugging a
			// step whose output they did not write.
			if !strings.Contains(err.Error(), "findings") {
				t.Errorf("error does not name the context key: %v", err)
			}
		})
	}
}
