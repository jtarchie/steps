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

func TestQuotedRefs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		tmpl string
		want []string
	}{
		{"a bare word", "echo {{ .args.x | shellquote }}", nil},
		{"after an option", "grep --term={{ .args.x | shellquote }} f", nil},
		{"inside double quotes", `echo "{{ .args.x | shellquote }}"`, []string{"{{.args.x | shellquote}}"}},
		{"inside single quotes", `echo '{{ .args.x | shellquote }}'`, []string{"{{.args.x | shellquote}}"}},
		{"after a closed pair", `echo "a" '{{ "b" }}' {{ .args.x | shellquote }}`, nil},
		{"an escaped quote does not open one", `echo \"{{ .args.x | shellquote }}`, nil},
		{"an escaped quote inside double quotes does not close it", `echo "a\" {{ .args.x | shellquote }}"`, []string{"{{.args.x | shellquote}}"}},
		{"a single quote inside double quotes is text", `echo "it's" {{ .args.x | shellquote }}`, nil},
		{"unwatched root", `echo "{{ .other.x | shellquote }}"`, nil},
		{"unquoted is the other scanner's", `echo "{{ .args.x }}"`, nil},
		{"heredoc body", "cat <<EOF > n\n{{ .args.x | shellquote }}\nEOF", []string{"{{.args.x | shellquote}}"}},
		{"heredoc after the value", "echo {{ .args.x | shellquote }}; cat <<EOF\nhi\nEOF", []string{"{{.args.x | shellquote}}"}},
		{"backticks", "echo `date` {{ .args.x | shellquote }}", []string{"{{.args.x | shellquote}}"}},
		{"ansi-c string", "echo $'a' {{ .args.x | shellquote }}", []string{"{{.args.x | shellquote}}"}},
		{"eval", "eval echo {{ .args.x | shellquote }}", []string{"{{.args.x | shellquote}}"}},
		{"nested sh -c", "bash -c {{ .args.x | shellquote }}", []string{"{{.args.x | shellquote}}"}},
		{"a word that merely contains eval", "retrieval {{ .args.x | shellquote }}", nil},
		{"inside an if", `{{ if .args.y }}echo "{{ .args.x | shellquote }}"{{ end }}`, []string{"{{.args.x | shellquote}}"}},
	} {
		got := QuotedRefs(tc.tmpl, "args")
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: QuotedRefs(%q) = %v, want %v", tc.name, tc.tmpl, got, tc.want)
		}
	}
}
