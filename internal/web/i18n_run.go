package web

import (
	"fmt"
	"html"
	"html/template"
	"reflect"
	"strings"
	"time"
)

// runFuncs are the run and node pages' template funcs that need a locale or build the markup a sentence takes as an argument. Added after templateFuncs, so the loc-aware JSON views replace the English ones there.
func runFuncs(loc *locale) template.FuncMap {
	esc := html.EscapeString

	return template.FuncMap{
		"jsonValue": func(raw string) jsonView { return jsonValueIn(loc, raw) },
		"lazyValue": func(src string, seq int64, raw string) jsonView { return lazyValueIn(loc, src, seq, raw) },
		// html/template writes an apostrophe in a string as &#39;; the markup this page always had carried it bare, so body text with one comes through here.
		"runtext": func(key string, args ...any) template.HTML {
			return template.HTML(strings.ReplaceAll(esc(loc.text(key, args...)), "&#39;", "'")) //nolint:gosec // escaped
		},
		"runlink": func(href, text string) template.HTML {
			return template.HTML(`<a href="` + esc(href) + `">` + esc(text) + `</a>`) //nolint:gosec // escaped
		},
		"runhash": func(href, text string) template.HTML {
			return template.HTML(`<a class="hash" href="` + esc(href) + `">` + esc(text) + `</a>`) //nolint:gosec // escaped
		},
		"runkbd":    func(key string) template.HTML { return template.HTML("<kbd>" + esc(key) + "</kbd>") },                   //nolint:gosec // escaped
		"runstrong": func(text string) template.HTML { return template.HTML("<b>" + esc(text) + "</b>") },                     //nolint:gosec // escaped
		"runident":  func(text string) template.HTML { return template.HTML(`<span class="ident">` + esc(text) + `</span>`) }, //nolint:gosec // escaped
		"runspan": func(title, text string) template.HTML {
			return template.HTML(`<span title="` + esc(title) + `">` + esc(text) + `</span>`) //nolint:gosec // escaped
		},
		"countdown": func(deadline time.Time, remaining time.Duration) template.HTML {
			return template.HTML(`<time class="live countdown" data-deadline="` + deadline.UTC().Format(time.RFC3339) + `">` + esc(formatDuration(remaining)) + `</time>`) //nolint:gosec // machine-formatted, escaped
		},
		"costTag": func(priced bool, cost any) template.HTML {
			if !priced {
				return unpriced(loc)
			}

			return template.HTML(esc(fmt.Sprint(cost))) //nolint:gosec // escaped
		},
		"costOrUnpriced": func(cost any) template.HTML {
			if value := reflect.ValueOf(cost); !value.IsValid() || value.IsZero() {
				return unpriced(loc)
			}

			return template.HTML(esc(fmt.Sprint(cost))) //nolint:gosec // escaped
		},
		"edgeLinks": func(slug string, edges []edgeView) template.HTML {
			var out strings.Builder

			for _, edge := range edges {
				out.WriteString(`<a href="/p/` + esc(slug) + `/jobs/` + esc(edge.Job) + `">` + esc(edge.Job) + `</a> <span class="res">[` + esc(edge.Resource) + `]</span> `)
			}

			return template.HTML(out.String()) //nolint:gosec // escaped
		},
	}
}

func unpriced(loc *locale) template.HTML {
	return template.HTML(`<span class="unpriced">` + html.EscapeString(loc.text("run.unpriced")) + `</span>`) //nolint:gosec // escaped
}
