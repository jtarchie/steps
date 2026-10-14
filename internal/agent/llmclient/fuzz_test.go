package llmclient

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzRepairJSONArgs(f *testing.F) {
	for _, seed := range []string{
		`{"a":1}`, `{"a":1,}`, `{"a":"x`, `{"a":`, `{"a":[1,2`, `junk {"a":1} tail`, `{"a":1}}`, `[1,2,]`,
		`{"s":"a\"b",`, `{"a":{"b":[1,{"c":`, ``, `   `, `null`, `"{"`, `{]`,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		out, changed := repairJSONArgs(raw)
		if !changed {
			if out != raw {
				t.Fatalf("repairJSONArgs(%q) = %q, false: an unchanged answer must be the input", raw, out)
			}

			return
		}

		if json.Valid([]byte(strings.TrimSpace(raw))) {
			t.Fatalf("repairJSONArgs(%q) rewrote input that already parses into %q", raw, out)
		}

		if !json.Valid([]byte(out)) || out == raw {
			t.Fatalf("repairJSONArgs(%q) = %q, true: a repair must parse and differ", raw, out)
		}

		again, changedAgain := repairJSONArgs(out)
		if changedAgain || again != out {
			t.Fatalf("repairJSONArgs is not idempotent: %q -> %q -> %q", raw, out, again)
		}
	})
}

func FuzzScanJSONObject(f *testing.F) {
	for _, seed := range []string{`{}`, `{"a":"}"}`, `{"a":[1,{"b":2}]} tail`, `{"a":"\"}`, `{`, `{]`} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if !strings.HasPrefix(s, "{") {
			return
		}

		got, complete := scanJSONObject(s)
		if problem := scanShapeProblem(s, got, complete); problem != "" {
			t.Fatalf("scanJSONObject(%q) = %q, %v: %s", s, got, complete, problem)
		}

		if json.Valid([]byte(s)) && (got != strings.TrimRight(s, " \t\r\n") || !complete) {
			t.Fatalf("scanJSONObject(%q) = %q, %v on a whole valid object", s, got, complete)
		}
	})
}

func scanShapeProblem(s, got string, complete bool) string {
	switch {
	case !strings.HasPrefix(s, got) || got == "":
		return "not a non-empty prefix"
	case complete && !strings.HasSuffix(got, "}") && !strings.HasSuffix(got, "]"):
		return "complete but not ended by a closer"
	case !complete && got != s:
		return "incomplete but not the whole input"
	default:
		return ""
	}
}

func FuzzStripTrailingCommas(f *testing.F) {
	for _, seed := range []string{`[1,]`, `{"a":1 , }`, `{"a":",]"}`, `{"a":"\",}"}`, `[[1,],]`, `[1,2]`} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got := stripTrailingCommas(s)
		if len(got) > len(s) {
			t.Fatalf("stripTrailingCommas(%q) = %q grew", s, got)
		}

		if json.Valid([]byte(s)) && got != s {
			t.Fatalf("stripTrailingCommas(%q) = %q changed valid JSON", s, got)
		}

		if strings.ReplaceAll(got, ",", "") != strings.ReplaceAll(s, ",", "") {
			t.Fatalf("stripTrailingCommas(%q) = %q removed something other than a comma", s, got)
		}
	})
}

func FuzzRepairChatCompletionBody(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{\"a\":1,"}}]}}]}`,
		`{"choices":[{"message":{"tool_calls":[{"function":{"name":"x","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":3}}`,
		`{"choices":null}`, `{"choices":[{"message":{"tool_calls":[{"function":{"arguments":7}}]}}]}`, `[]`, `nope`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		got := repairChatCompletionBody(body)
		if !json.Valid(body) {
			if string(got) != string(body) {
				t.Fatalf("repairChatCompletionBody rewrote a body that does not parse: %q -> %q", body, got)
			}

			return
		}

		if !json.Valid(got) {
			t.Fatalf("repairChatCompletionBody(%q) = %q, no longer JSON", body, got)
		}

		if again := repairChatCompletionBody(got); string(again) != string(got) {
			t.Fatalf("repairChatCompletionBody is not idempotent: %q -> %q -> %q", body, got, again)
		}
	})
}

func FuzzSanitizeLabel(f *testing.F) {
	for _, seed := range []string{"build job", "déploy", strings.Repeat("x", 100), "a\r\nX-Injected: 1"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got := sanitizeLabel(name)
		if len(got) > maxLabelLen {
			t.Fatalf("sanitizeLabel(%q) = %d bytes", name, len(got))
		}

		for _, r := range got {
			if !strings.ContainsRune(labelAlphabet, r) {
				t.Fatalf("sanitizeLabel(%q) = %q, holding %q", name, got, r)
			}
		}

		if sanitizeLabel(got) != got {
			t.Fatalf("sanitizeLabel is not idempotent on %q", got)
		}
	})
}

const labelAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"
