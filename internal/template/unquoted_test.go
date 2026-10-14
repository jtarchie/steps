package template

import (
	"slices"
	"testing"
)

func TestUnquotedRefs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		tmpl string
		want []string
	}{
		{"bare field", "git checkout {{ .version.ref }}", []string{"{{.version.ref}}"}},
		{"piped through shellquote", "git checkout {{ .version.ref | shellquote }}", nil},
		{"shellquote as a call", "git checkout {{ shellquote .version.ref }}", nil},
		{"shellquote not last", "echo {{ .version.ref | shellquote | upper }}", []string{"{{.version.ref | shellquote | upper}}"}},
		{"through another function", `echo {{ index .source "branch" | default "HEAD" }}`, []string{`{{index .source "branch" | default "HEAD"}}`}},
		{"quoted through index", `echo {{ index .source "branch" | default "HEAD" | shellquote }}`, nil},
		{"unwatched root", "echo {{ .other.x }}", nil},
		{"condition is not output", "{{ if .version.ref }}echo hi{{ end }}", nil},
		{"inside an if body", "{{ if .source.x }}echo {{ .params.y }}{{ end }}", []string{"{{.params.y}}"}},
		{"else body", "{{ if .x }}a{{ else }}echo {{ .params.y }}{{ end }}", []string{"{{.params.y}}"}},
		{"with rebinds dot to a root", "{{ with .version }}echo {{ .ref }}{{ end }}", []string{"{{.ref}}"}},
		{"range over a root", "{{ range .params.files }}rm {{ . }}{{ end }}", []string{"{{.}}"}},
		{"range quoted", "{{ range .params.files }}rm {{ . | shellquote }}{{ end }}", nil},
		{"with over a non-root", "{{ with .other }}echo {{ .ref }}{{ end }}", nil},
		{"dollar reaches the root", "{{ with .other }}echo {{ $.version.ref }}{{ end }}", []string{"{{$.version.ref}}"}},
		{"variable from a root", "{{ $r := .version.ref }}echo {{ $r }}", []string{"{{$r}}"}},
		{"variable quoted", "{{ $r := .version.ref }}echo {{ $r | shellquote }}", nil},
		{"parenthesized", "echo {{ (printf \"%s\" .args.x) }}", []string{"{{(printf \"%s\" .args.x)}}"}},
		{"args root", "gh pr comment {{ .args.body }}", []string{"{{.args.body}}"}},
		{"repeated once", "echo {{ .args.a }} {{ .args.a }}", []string{"{{.args.a}}"}},
		{"unparsable", "echo {{ .args.a ", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := UnquotedRefs(tc.tmpl, "source", "version", "params", "args")
			if !slices.Equal(got, tc.want) {
				t.Errorf("UnquotedRefs(%q) = %q, want %q", tc.tmpl, got, tc.want)
			}
		})
	}
}
