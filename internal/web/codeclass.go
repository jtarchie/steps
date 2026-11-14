package web

import (
	"fmt"
	"html"
	"io"
	"iter"

	"github.com/alecthomas/chroma/v3"
)

// classFormatter draws a token docsCodeStyle styles as <span class="hl-…"> naming the style entry it inherits from, and any other token as bare escaped text, so app.css colors code from the theme's tokens. chroma's own class mode spans every token, styled or not (measured +21% on YAML and +48% on Go over inline styles), and its inline mode writes one theme's hex into the markup.
type classFormatter struct {
	pre bool
}

// Format recovers a lexer's panic into an error, as chroma's Formatter contract asks: the docs fence path has no recover of its own.
func (f classFormatter) Format(w io.Writer, style *chroma.Style, tokens iter.Seq[chroma.Token]) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("highlight: lexer panicked: %v", r)
		}
	}()

	if f.pre {
		_, err = io.WriteString(w, `<pre class="hl"><code>`)
		if err != nil {
			return fmt.Errorf("highlight: %w", err)
		}
	}

	for token := range tokens {
		text := html.EscapeString(token.Value)

		if class := styledClass(style, token.Type); class != "" {
			_, err = fmt.Fprintf(w, `<span class="hl-%s">%s</span>`, class, text)
		} else {
			_, err = io.WriteString(w, text)
		}

		if err != nil {
			return fmt.Errorf("highlight: %w", err)
		}
	}

	if f.pre {
		_, err = io.WriteString(w, `</code></pre>`)
		if err != nil {
			return fmt.Errorf("highlight: %w", err)
		}
	}

	return nil
}

// styledClass is chroma's short class for the nearest of tokenType and its parents that style has an entry for, or "" when none does.
func styledClass(style *chroma.Style, tokenType chroma.TokenType) string {
	for t := tokenType; t != 0; t = t.Parent() {
		if style.Has(t) {
			return chroma.StandardTypes[t]
		}
	}

	return ""
}
