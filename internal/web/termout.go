package web

import (
	"html/template"
	"strings"

	terminal "github.com/jtarchie/terminal-to-html/v3"

	"github.com/jtarchie/steps/internal/runview"
)

// outputPre draws recorded process output. Only text a terminal would act on (an escape or a carriage return) goes through the emulator; the rest keeps jsonPre's JSON and diff highlighting.
func outputPre(raw string) template.HTML {
	if !strings.ContainsAny(raw, "\x1b\r") {
		return jsonPre(runview.Scrub(raw))
	}

	return terminalHTML(raw)
}

// outputChunk draws one live chunk. The emulator trims the line ending a chunk closes on, and the chunks are concatenated spans, so it goes back.
// ponytail: a fresh screen per chunk, so a colour left open across a chunk boundary is lost until the finished row re-renders the whole output; hold one screen per streaming step in the framer if that shows.
func outputChunk(raw string) template.HTML {
	if !strings.ContainsAny(raw, "\x1b\r") {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(runview.Scrub(raw)))
	}

	html := terminalHTML(raw)
	if strings.HasSuffix(raw, "\n") || strings.HasSuffix(raw, "\r") {
		html += "\n"
	}

	return html
}

// terminalHTML renders output that may come from an untrusted branch: strict mode drops inline images and keeps only http(s) links, text is escaped by the library, and the control bytes it passes through (NUL, BEL, C1) are dropped here as runview.Scrub drops them. The default 160x100 screen bounds what a cursor jump can allocate; it does not wrap long lines.
func terminalHTML(raw string) template.HTML {
	screen, err := terminal.NewScreen(terminal.WithStrict())
	if err != nil {
		//nolint:gosec // G203: escaped, no markup added
		return template.HTML(template.HTMLEscapeString(runview.Scrub(raw)))
	}

	screen.Timestamps = false
	_, _ = screen.Write([]byte(raw))

	//nolint:gosec // G203: the library escapes text and emits only its own class spans and sanitized http(s) links
	return template.HTML(strings.Map(dropControl, screen.AsHTML()))
}

func dropControl(r rune) rune {
	if (r < 0x20 && r != '\t' && r != '\n') || (r >= 0x7f && r < 0xa0) {
		return -1
	}

	return r
}
