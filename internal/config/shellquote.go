package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// acrossFuncs is what an across: template may call. shellquote is the one an
// item written mid-run needs: a from_file: value is often model output, and a
// run: line is handed to sh -c.
//
//nolint:gochecknoglobals // read-only
var acrossFuncs = template.FuncMap{"shellquote": shellQuote}

func newAcrossTemplate() *template.Template {
	return template.New("across").Funcs(acrossFuncs)
}

var (
	errShellquoteNull = errors.New("shellquote: a value containing a null byte cannot be quoted")

	//nolint:gochecknoglobals // compiled once, read-only
	shellUnsafe = regexp.MustCompile(`[^\w!%+,\-./:=@^]`)
	//nolint:gochecknoglobals // compiled once, read-only
	escapedQuoteRun = regexp.MustCompile(`(?:'\\''){2,}`)
)

// shellQuote is internal/template's shellquote — leatherman's Quote for a
// single word — ported because config may import nothing but the standard
// library and yaml. TestShellQuoteMatchesLeatherman holds the two together.
//
// A word containing '=' is quoted even when otherwise safe: as the first word
// of a command, `a=b` would be an assignment rather than an argument.
func shellQuote(v any) (string, error) {
	word := fmt.Sprint(v)

	if word == "" {
		return "''", nil
	}

	if strings.Contains(word, "\x00") {
		return "", errShellquoteNull
	}

	if !strings.Contains(word, "=") && !shellUnsafe.MatchString(word) {
		return word, nil
	}

	quoted := strings.ReplaceAll(word, `'`, `'\''`)
	quoted = escapedQuoteRun.ReplaceAllStringFunc(quoted, func(run string) string {
		return `'"` + strings.Repeat("'", len(run)/4) + `"'`
	})

	quoted = strings.TrimPrefix(strings.TrimSuffix("'"+quoted+"'", "''"), "''")

	return quoted, nil
}
