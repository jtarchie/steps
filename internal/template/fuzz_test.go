package template

import (
	"errors"
	"strings"
	"testing"
)

// posixWord is this test's own POSIX sh reading of one word: what `sh -c` would pass as argv, or an error if the text is not exactly one word with nothing the shell would act on.
//
//nolint:cyclop // a shell lexer: one branch per quoting rule
func posixWord(text string) (string, error) {
	const bare = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_!%+,-./:@^"

	var out strings.Builder

	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == '\'':
			end := strings.IndexByte(text[i+1:], '\'')
			if end < 0 {
				return "", errors.New("unterminated single quote")
			}

			out.WriteString(text[i+1 : i+1+end])
			i += end + 2
		case c == '\\':
			if i+1 >= len(text) {
				return "", errors.New("trailing backslash")
			}

			if text[i+1] != '\n' {
				out.WriteByte(text[i+1])
			}

			i += 2
		case c == '"':
			i++

			for ; i < len(text) && text[i] != '"'; i++ {
				if strings.IndexByte("$`", text[i]) >= 0 {
					return "", errors.New("expansion inside double quotes")
				}

				if text[i] == '\\' && i+1 < len(text) && strings.IndexByte("$`\"\\\n", text[i+1]) >= 0 {
					i++
				}

				out.WriteByte(text[i])
			}

			if i >= len(text) {
				return "", errors.New("unterminated double quote")
			}

			i++
		case strings.IndexByte(bare, c) >= 0:
			out.WriteByte(c)
			i++
		default:
			return "", errors.New("unquoted shell metacharacter " + string(c))
		}
	}

	return out.String(), nil
}

// FuzzShellquote renders a value through a template's shellquote the way a command line does, and holds it to the shell's reading: one word, carrying the value exactly.
func FuzzShellquote(f *testing.F) {
	f.Add("approve")
	f.Add("")
	f.Add("it's $(rm -rf /) `x` ; | & > < * ? [a] ~ # \\ \" \n\t")
	f.Add("a''b")
	f.Add("foo=bar")

	f.Fuzz(func(t *testing.T, value string) {
		rendered, err := Render(`gh pr review --body {{ shellquote .body }}`, map[string]any{"body": value})
		if strings.ContainsRune(value, 0) {
			if err == nil {
				t.Fatalf("a NUL byte rendered: %q", rendered)
			}

			return
		}

		if err != nil {
			t.Fatalf("shellquote %q: %v", value, err)
		}

		word, found := strings.CutPrefix(rendered, "gh pr review --body ")
		if !found {
			t.Fatalf("rendered %q", rendered)
		}

		got, err := posixWord(word)
		if err != nil {
			t.Fatalf("%q quoted as %q: %v", value, word, err)
		}

		if got != value {
			t.Fatalf("%q quoted as %q, which the shell reads as %q", value, word, got)
		}
	})
}

// FuzzRenderLiteral: text with no action in it renders as itself, whatever the data holds.
func FuzzRenderLiteral(f *testing.F) {
	f.Add("echo hello", "x")
	f.Add("{ } }} {", "y")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, text, value string) {
		if strings.Contains(text, "{{") {
			return
		}

		got, err := Render(text, map[string]any{"source": map[string]any{"v": value}})
		if err != nil || got != text {
			t.Fatalf("Render(%q) = %q, %v; want the text unchanged", text, got, err)
		}
	})
}
