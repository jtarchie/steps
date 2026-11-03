package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func FuzzEntryStorePath(f *testing.F) {
	f.Add("0123456789abcdef")
	f.Add("..")
	f.Add("a/b")
	f.Add(`a\b`)
	f.Add("")

	store := &entryStore{dir: filepath.Join("cache", "entries")}

	f.Fuzz(func(t *testing.T, key string) {
		got, ok := store.path(key)
		if !ok {
			return
		}

		if filepath.Dir(got) != store.dir || filepath.Base(got) != key {
			t.Fatalf("key %q became %q, not an entry directly under %q", key, got, store.dir)
		}
	})
}

func FuzzSanitizeLabel(f *testing.F) {
	f.Add("build")
	f.Add("..")
	f.Add("a/b\\c d")
	f.Add("")
	f.Add("日本")

	f.Fuzz(func(t *testing.T, label string) {
		got := sanitizeLabel(label)

		if got == "" || strings.ContainsAny(got, `/\`) || got != sanitizeLabel(got) {
			t.Fatalf("sanitizeLabel(%q) = %q, not a stable single path element", label, got)
		}

		if filepath.Base("01-"+got) != "01-"+got {
			t.Fatalf("label %q escapes the step directory it names", label)
		}
	})
}

func FuzzValidateCachedNames(f *testing.F) {
	f.Add("repo", "out", "renamed")
	f.Add("..", "out", "")
	f.Add("repo", "out", "../escape")
	f.Add("repo", "out", "findings/alpha/fast")

	f.Fuzz(func(t *testing.T, input, output, mapped string) {
		req := StepCacheRequest{Inputs: []string{input}, Outputs: []string{output}}
		if mapped != "" {
			req.OutputMapping = map[string]string{output: mapped}
		}

		if validateCachedNames(req) != nil {
			return
		}

		for _, name := range []string{input, output, mappedName(output, req.OutputMapping)} {
			if !filepath.IsLocal(name) || filepath.Clean(name) != name || strings.Contains(name, `\`) {
				t.Fatalf("accepted %q, which does not stay under the directory it is joined to", name)
			}
		}

		if strings.Contains(input, "/") || strings.Contains(output, "/") {
			t.Fatalf("accepted a declared name with a separator: %q / %q", input, output)
		}
	})
}
