package web

import (
	"net/url"
	"strings"
)

// listView is what every list page draws above its table — a search box, chips narrowing by one field, the narrowings in force each removable, the cursor's links — so a reader who learned the runs page has learned them all.
type listView struct {
	Base  string
	Query url.Values
	// Text is the search box's value, trimmed.
	Text string
	// Hint and Label are the box's placeholder and accessible name, already in the reader's words.
	Hint    string
	Label   string
	Facets  []facetGroup
	Filters []activityFilter
	Older   string
	Paged   bool
}

// facetGroup is one field's chips. All is first and current while the field is unset.
type facetGroup struct {
	Label   string
	Name    string
	Options []facetOption
}

type facetOption struct {
	Label   string
	URL     string
	Current bool
	// Foreign is a word the configuration chose, drawn as English on a translated page.
	Foreign bool
}

func newList(base string, query url.Values, hint, label string) *listView {
	return &listView{
		Base:  base,
		Query: query,
		Text:  strings.TrimSpace(query.Get("q")),
		Hint:  hint,
		Label: label,
		Paged: query.Get("before") != "",
	}
}

// facet adds key's chips, one per value, each drawn by show. A field with fewer than two values to choose between draws no chips, but a value already chosen still narrows and is still listed as removable.
func (l *listView) facet(loc *locale, key string, values []string, show func(string) string) string {
	current := l.Query.Get(key)

	if current != "" {
		l.Filters = append(l.Filters, activityFilter{Label: key, Value: show(current), Remove: pathQuery(l.Base, l.Query, key, "")})
	}

	if len(values) < 2 {
		return current
	}

	label := loc.word("filter", key)
	group := facetGroup{Label: label, Name: loc.text("facet.label", label)}
	group.Options = append(group.Options, facetOption{Label: loc.text("facet.all"), URL: pathQuery(l.Base, l.Query, key, ""), Current: current == ""})

	for _, value := range values {
		group.Options = append(group.Options, facetOption{Label: show(value), URL: pathQuery(l.Base, l.Query, key, value), Current: current == value})
	}

	l.Facets = append(l.Facets, group)

	return current
}

// configFacet is facet for values the configuration spells — a transport, an auth type — which stay as written in every language.
func (l *listView) configFacet(loc *locale, key string, values []string) string {
	before := len(l.Facets)
	current := l.facet(loc, key, values, identity)

	if len(l.Facets) > before {
		options := l.Facets[len(l.Facets)-1].Options
		for i := 1; i < len(options); i++ {
			options[i].Foreign = true
		}
	}

	return current
}

// searched lists the box's text as a removable narrowing; called after every facet so it reads last, as it is typed last.
func (l *listView) searched() {
	if l.Text != "" {
		l.Filters = append(l.Filters, activityFilter{Label: "text", Value: l.Text, Remove: pathQuery(l.Base, l.Query, "q", "")})
	}
}

// page trims rows to size and sets Older to the cursor after the last one kept, when there were more.
func page[T any](l *listView, rows []T, size int, cursor func(T) string) []T {
	if len(rows) <= size {
		return rows
	}

	rows = rows[:size]
	l.Older = pathQuery(l.Base, l.Query, "before", cursor(rows[len(rows)-1]))

	return rows
}

// matches is the page-side text search for a list small enough to filter in Go: any field holding text, ignoring case.
func matches(text string, fields ...string) bool {
	if text == "" {
		return true
	}

	needle := strings.ToLower(text)

	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), needle) {
			return true
		}
	}

	return false
}
