package pipeline

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// FuzzDecodeAcrossItems holds the decoder to its own promise, checked by a decoder that cannot be lenient: an axis is a JSON array whose every element is a string.
func FuzzDecodeAcrossItems(f *testing.F) {
	for _, seed := range []string{`["a","b"]`, `[]`, `null`, `["a",null]`, `[1]`, `{"a":1}`, `"a"`, `["a"] x`, ` [ "é" ] `, ``} {
		f.Add([]byte(seed))
	}

	f.Add([]byte("[" + strings.Repeat(`"x",`, config.MaxAcrossItems) + `"x"]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		items, err := decodeAcrossItems("axis.json", data)

		var loose any

		strict := json.Unmarshal(data, &loose) == nil

		elements, isArray := loose.([]any)
		strict = strict && isArray && len(elements) <= config.MaxAcrossItems

		want := make([]string, 0, len(elements))

		for _, element := range elements {
			s, ok := element.(string)
			strict = strict && ok
			want = append(want, s)
		}

		if (err == nil) != strict {
			t.Fatalf("decodeAcrossItems(%q) = %q, %v; an array of at most %d strings is the only thing it may accept", data, items, err, config.MaxAcrossItems)
		}

		if err == nil && !slices.Equal(items, want) {
			t.Fatalf("decodeAcrossItems(%q) = %q, want %q", data, items, want)
		}
	})
}
