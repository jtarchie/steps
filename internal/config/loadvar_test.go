package config

import (
	"slices"
	"strings"
	"testing"
)

// TestJSONWithTrailingTextIsRefused: decoder.More() answers false before a stray } or ], so a check on it alone let `{"a":1}}` through as a valid file.
func TestJSONWithTrailingTextIsRefused(t *testing.T) {
	t.Parallel()

	for _, body := range []string{`{"a":1}}`, `{"a":1}]`, `{"a":1} {"b":2}`} {
		_, err := ParseLoadVar(LoadVarJSON, []byte(body))
		if err == nil {
			t.Errorf("ParseLoadVar(json, %q) succeeded, want trailing text refused", body)
		}
	}

	_, err := ParseLoadVar(LoadVarJSON, []byte("{\"a\":1}\n"))
	if err != nil {
		t.Errorf("a json file ending in a newline was refused: %v", err)
	}
}

// TestYAMLKeysAreStrings: yaml.v4 decodes a mapping with a non-string key as map[any]any, which no field path reached, no mask saw into, and json could not encode — so reveal: true failed the step.
func TestYAMLKeysAreStrings(t *testing.T) {
	t.Parallel()

	value, err := ParseLoadVar(LoadVarYAML, []byte("ports:\n  80: http\n  443: hidden-token-value\n"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := LocalValue(value, LocalRef{Root: "cfg", Fields: []string{"ports", "80"}})
	if err != nil || got != "http" {
		t.Errorf("((.:cfg.ports.80)) = (%q, %v), want http", got, err)
	}

	whole, err := LocalValue(value, LocalRef{Root: "cfg"})
	if err != nil || !strings.Contains(whole, `"443":"hidden-token-value"`) {
		t.Errorf("((.:cfg)) = (%s, %v), want the numeric key spelled as a string", whole, err)
	}

	if !slices.Contains(LocalStrings(value), "hidden-token-value") {
		t.Errorf("masking sees %q, want the value under a numeric key", LocalStrings(value))
	}
}

// TestAWholeValueIsNotHTMLEscaped: json.Marshal turns & < > into & < >, which changed what a command received and kept a masked string from matching.
func TestAWholeValueIsNotHTMLEscaped(t *testing.T) {
	t.Parallel()

	value, err := ParseLoadVar(LoadVarJSON, []byte(`{"url":"https://x/?a=1&b=<2>"}`))
	if err != nil {
		t.Fatal(err)
	}

	got, err := LocalValue(value, LocalRef{Root: "cfg"})
	if err != nil || got != `{"url":"https://x/?a=1&b=<2>"}` {
		t.Errorf("((.:cfg)) = (%s, %v), want the text as written", got, err)
	}
}
