package template

import "testing"

func TestRender(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"source":  map[string]any{"repo": "jtarchie/ci"},
		"version": map[string]any{"number": 87},
	}

	t.Run("renders fields", func(t *testing.T) {
		t.Parallel()

		got, err := Render("clone {{ .source.repo }} at {{ .version.number }}", data)
		if err != nil {
			t.Fatal(err)
		}

		if want := "clone jtarchie/ci at 87"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("errors on missing key", func(t *testing.T) {
		t.Parallel()

		_, err := Render("{{ .source.nope }}", data)
		if err == nil {
			t.Error("expected error for missing key")
		}
	})

	t.Run("errors on malformed template", func(t *testing.T) {
		t.Parallel()

		_, err := Render("{{ .source.repo ", data)
		if err == nil {
			t.Error("expected error for malformed template")
		}
	})
}

func TestRenderShellquote(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tmpl string
		data map[string]any
		want string
	}{
		{
			// The whole body is one single-quoted word; the embedded ' becomes '\''.
			name: "neutralizes shell metacharacters",
			tmpl: `gh pr review -b {{ .args.body | shellquote }}`,
			data: map[string]any{"args": map[string]any{"body": "a `replace` b $(id) c 'q' d"}},
			want: `gh pr review -b 'a ` + "`replace`" + ` b $(id) c '\''q'\'' d'`,
		},
		{
			name: "wraps an empty value",
			tmpl: `x {{ .empty | shellquote }} y`,
			data: map[string]any{"empty": ""},
			want: "x '' y",
		},
		{
			// A value with no shell-special characters needs no quoting, so the
			// rendered command stays readable (e.g. --approve, not --'approve').
			name: "leaves an already-safe value bare",
			tmpl: `gh pr review --{{ .action | shellquote }}`,
			data: map[string]any{"action": "approve"},
			want: "gh pr review --approve",
		},
		{
			// shellquote composes with sprig pipelines: sprig transforms the
			// value, shellquote makes the result shell-safe.
			name: "composes with a sprig function",
			tmpl: `msg {{ .body | trim | shellquote }}`,
			data: map[string]any{"body": "  hi `x` there  "},
			want: "msg 'hi `x` there'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Render(tt.tmpl, tt.data)
			if err != nil {
				t.Fatal(err)
			}

			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
