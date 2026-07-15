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

	t.Run("shellquote neutralizes shell metacharacters", func(t *testing.T) {
		t.Parallel()

		args := map[string]any{"args": map[string]any{
			"body": "a `replace` b $(id) c 'q' d",
		}}

		got, err := Render(`gh pr review -b {{ .args.body | shellquote }}`, args)
		if err != nil {
			t.Fatal(err)
		}

		// The whole body is one single-quoted word; the embedded ' becomes '\''.
		want := `gh pr review -b 'a ` + "`replace`" + ` b $(id) c '\''q'\'' d'`
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("shellquote wraps an empty value", func(t *testing.T) {
		t.Parallel()

		got, err := Render(`x {{ .empty | shellquote }} y`, map[string]any{"empty": ""})
		if err != nil {
			t.Fatal(err)
		}

		if want := "x '' y"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
