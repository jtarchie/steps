package web

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/alecthomas/chroma/v3"
)

// TestCodeClassesFollowTheStyle: docsCodeStyle decides which tokens are colored and how; app.css's highlight block is that decision written as rules on the theme's tokens. The block is generated here from the style — each hex mapped to the dark token it equals — and must match app.css byte for byte, so a style entry added, recolored or dropped fails until the CSS says the same thing. A color with no token fails too: code may only be drawn in the palette.
func TestCodeClassesFollowTheStyle(t *testing.T) {
	t.Parallel()

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	const begin, end = "/* hl:begin", "/* hl:end */"

	from, to := strings.Index(string(css), begin), strings.Index(string(css), end)
	if from < 0 || to < from {
		t.Fatalf("app.css has no %q … %q block", begin, end)
	}

	have := string(css[strings.Index(string(css[from:]), "\n")+from+1 : to])

	want := highlightCSS(t, themeTokens(t, css)[1])

	if have != want {
		t.Errorf("app.css's highlight block does not match docsCodeStyle; replace it with:\n%s", want)
	}
}

// highlightCSS writes docsCodeStyle as app.css rules: one per styled token type, each color named by the dark token it equals.
func highlightCSS(t *testing.T, dark map[string]string) string {
	t.Helper()

	byHex := map[string]string{}
	for _, name := range []string{"cyan", "magenta", "blue", "yellow", "red", "green", "dim", "fg", "panel", "code-bg", "bg"} {
		byHex[dark[name]] = name
	}

	token := func(colour chroma.Colour) string {
		name, ok := byHex[colour.String()]
		if !ok {
			t.Errorf("docsCodeStyle draws %s, which is no token in app.css", colour)
		}

		return "var(--" + name + ")"
	}

	background := docsCodeStyle.Get(chroma.Background)

	var out strings.Builder

	fmt.Fprintf(&out, "pre.hl { color: %s; background: %s; }\n", token(background.Colour), token(background.Background))

	types := docsCodeStyle.Types()
	slices.SortFunc(types, func(a, b chroma.TokenType) int { return cmp.Compare(chroma.StandardTypes[a], chroma.StandardTypes[b]) })

	for _, tokenType := range types {
		class := chroma.StandardTypes[tokenType]
		if tokenType == chroma.Background || class == "" {
			continue
		}

		entry := docsCodeStyle.Get(tokenType)

		var decls []string
		if entry.Colour != background.Colour {
			decls = append(decls, "color: "+token(entry.Colour))
		}

		if entry.Bold == chroma.Yes {
			decls = append(decls, "font-weight: bold")
		}

		if entry.Italic == chroma.Yes {
			decls = append(decls, "font-style: italic")
		}

		fmt.Fprintf(&out, ".hl-%s { %s; }\n", class, strings.Join(decls, "; "))
	}

	return out.String()
}

// TestHighlightSpansOnlyStyledTokens: a token the style colors is one span naming its style entry, even when the lexer emitted a subtype of it; a token it does not is bare text. Spanning every token is what chroma's own class mode does, and it grew a highlighted Go file by half.
func TestHighlightSpansOnlyStyledTokens(t *testing.T) {
	t.Parallel()

	got := string(highlightCode("package main // hi\n", "go"))

	for _, want := range []string{`<span class="hl-k">package</span>`, `<span class="hl-c">// hi`} {
		if !strings.Contains(got, want) {
			t.Errorf("highlighted Go lacks %s:\n%s", want, got)
		}
	}

	if strings.Contains(got, `class="hl-w"`) || strings.Contains(got, `class="hl-nx"`) || strings.Contains(got, "style=") {
		t.Errorf("an unstyled token got markup, or a style is inline:\n%s", got)
	}
}
