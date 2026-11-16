package web

import (
	"github.com/labstack/echo/v5"
	"html"
	"html/template"
	"strings"
)

// foreign is text this page shows but does not own: a sentence from the config package, a provider, a server. English locale draws it as the template's own escaping would; any other marks it lang="en" so a screen reader says it in the right voice.
func (l *locale) foreign(text string) template.HTML {
	escaped := html.EscapeString(text)
	if l.tag == englishTag {
		return template.HTML(escaped) //nolint:gosec // escaped above
	}

	return template.HTML(`<span lang="en">` + escaped + `</span>`) //nolint:gosec // escaped above
}

// enAttr marks an element whose content stays English on a translated page; on an English page it adds nothing.
func (l *locale) enAttr() template.HTMLAttr {
	if l.tag == englishTag {
		return ""
	}

	return ` lang="en"`
}

// extLink is a link to somewhere else, for a sentence that takes it as an argument. Only http(s) hrefs are kept.
func extLink(href, text string) template.HTML {
	if !strings.HasPrefix(href, "https://") && !strings.HasPrefix(href, "http://") {
		href = "#"
	}

	return template.HTML(`<a href="` + html.EscapeString(href) + `" rel="noopener">` + html.EscapeString(text) + `</a>`) //nolint:gosec // both parts escaped, scheme checked
}

// mcpStatus is a status cell: the sentences this package writes are translated, those the config package and the token holder write (the same wording the CLI prints) are shown as they are.
func (l *locale) mcpStatus(text string) template.HTML {
	if _, ok := l.msgs["mcp.status."+text]; ok {
		return template.HTML(html.EscapeString(l.text("mcp.status." + text))) //nolint:gosec // escaped
	}

	return l.foreign(text)
}

// say is a message in the locale of the request being answered.
func (s *Server) say(c *echo.Context, key string, args ...any) string {
	return s.localeFor(c.Request()).text(key, args...)
}

// followWords is every word the follow page's script draws, handed over as data the way the copy button's is.
func (l *locale) followWords() map[string]any {
	return map[string]any{
		"states": map[string]string{
			"pending": l.text("status.queued"), "running": l.text("follow.starting"), "waiting": l.text("follow.waiting"),
			"failed": l.text("status.failed"), "aborted": l.text("status.aborted"),
		},
		"queued":   l.text("status.queued"),
		"failed":   l.text("follow.ended.failed"),
		"aborted":  l.text("follow.ended.aborted"),
		"stopped":  l.text("follow.stopped"),
		"lost":     l.text("follow.lost"),
		"clear":    l.text("follow.clear"),
		"blocking": l.text("follow.blocking"),
	}
}
