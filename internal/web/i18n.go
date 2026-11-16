package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/actor"
	"github.com/labstack/echo/v5"
	cldr "golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
)

//go:embed locales/*.json
var localeFiles embed.FS

const (
	englishTag = "en"
	// Test-only: English mangled so a word drawn outside a catalog stays plain ASCII.
	pseudoTag  = "en-XA"
	langCookie = "steps-lang"
	langPath   = "/lang"

	localeKey = "steps.locale"
)

// supported lists the production locales, in the order the matcher indexes them. the picker lists them in this order.
var supported = []language.Tag{language.English, language.MustParse("zh-CN"), language.Japanese, language.Spanish, language.MustParse("pt-BR")}

var matcher = language.NewMatcher(supported)

// locale is one language's catalog. A message names itself by key and takes its values by indexed verb (%[1]s), so a translator can reorder them.
type locale struct {
	tag    string
	msgs   map[string]string
	pseudo bool
}

var (
	localesMu sync.Mutex
	locales   = map[string]*locale{}
)

// loadLocale reads a locale once. The pseudo-locale is generated from English rather than shipped, so it can never drift from it.
func loadLocale(tag string) (*locale, error) {
	localesMu.Lock()
	defer localesMu.Unlock()

	if loc, ok := locales[tag]; ok {
		return loc, nil
	}

	file := tag
	if tag == pseudoTag {
		file = englishTag
	}

	loc := &locale{tag: tag, msgs: map[string]string{}, pseudo: tag == pseudoTag}

	raw, err := localeFiles.ReadFile("locales/" + file + ".json")
	if err != nil {
		return nil, fmt.Errorf("web: no catalog for %q: %w", tag, err)
	}

	err = json.Unmarshal(raw, &loc.msgs)
	if err != nil {
		return nil, fmt.Errorf("web: catalog %q: %w", tag, err)
	}

	locales[tag] = loc

	return loc, nil
}

// raw is the message's text, mangled in the pseudo-locale. A missing key renders as itself, which a test then sees as plain ASCII.
func (l *locale) raw(key string) string {
	text, ok := l.msgs[key]
	if !ok {
		return key
	}

	if l.pseudo {
		return pseudo(text)
	}

	return text
}

// text is a message with no markup in it. The caller's template escapes the result for wherever it lands (body or attribute).
func (l *locale) text(key string, args ...any) string {
	return sprintf(l.raw(key), args)
}

// html is a message with markup in the middle. Catalog text and every plain argument are escaped here; only an argument the caller built as template.HTML is emitted as it is, so markup never comes from a catalog.
func (l *locale) html(key string, args ...any) template.HTML {
	safe := make([]any, len(args))

	for i, arg := range args {
		if markup, ok := arg.(template.HTML); ok {
			safe[i] = string(markup)
		} else if str, ok := arg.(string); ok {
			safe[i] = html.EscapeString(str)
		} else {
			safe[i] = arg
		}
	}

	return template.HTML(sprintf(html.EscapeString(l.raw(key)), safe)) //nolint:gosec // text and plain arguments escaped above; markup is only what the caller marked safe
}

func sprintf(format string, args []any) string {
	if len(args) == 0 {
		return format
	}

	return fmt.Sprintf(format, args...)
}

var pseudoMap = map[rune]rune{
	'a': 'à', 'b': 'ƀ', 'c': 'ç', 'd': 'ð', 'e': 'é', 'f': 'ƒ', 'g': 'ĝ', 'h': 'ĥ', 'i': 'î', 'j': 'ĵ', 'k': 'ķ', 'l': 'ļ', 'm': 'ɱ',
	'n': 'ñ', 'o': 'ô', 'p': 'þ', 'q': 'ǫ', 'r': 'ŕ', 's': 'š', 't': 'ţ', 'u': 'û', 'v': 'ṽ', 'w': 'ŵ', 'x': 'ẋ', 'y': 'ý', 'z': 'ž',
	'A': 'À', 'B': 'Ɓ', 'C': 'Ç', 'D': 'Ð', 'E': 'É', 'F': 'Ƒ', 'G': 'Ĝ', 'H': 'Ĥ', 'I': 'Î', 'J': 'Ĵ', 'K': 'Ķ', 'L': 'Ļ', 'M': 'Ṁ',
	'N': 'Ñ', 'O': 'Ô', 'P': 'Þ', 'Q': 'Ǫ', 'R': 'Ŕ', 'S': 'Š', 'T': 'Ţ', 'U': 'Û', 'V': 'Ṽ', 'W': 'Ŵ', 'X': 'Ẋ', 'Y': 'Ý', 'Z': 'Ž',
}

// pseudo mangles English into "[Ŕûñ···]", leaving %[n]verbs alone so the values still land.
func pseudo(text string) string {
	var out strings.Builder

	out.WriteByte('[')

	inVerb := false

	for _, r := range text {
		switch {
		case r == '%':
			inVerb = true
			out.WriteRune(r)
		case inVerb:
			out.WriteRune(r)

			if r >= 'a' && r <= 'z' || r == '%' {
				inVerb = false
			}
		default:
			if mapped, ok := pseudoMap[r]; ok {
				r = mapped
			}

			out.WriteRune(r)
		}
	}

	out.WriteString("···]")

	return out.String()
}

// localeFor is the visitor's locale: the cookie, then Accept-Language, then English. The matcher's INDEX picks the catalog, never the tag it returns, which comes back decorated (ja-u-rg-jpzzzz).
func (s *Server) localeFor(r *http.Request) *locale {
	tag := s.pickTag(r)

	loc, err := loadLocale(tag)
	if err != nil {
		return englishLocale()
	}

	return loc
}

func (s *Server) pickTag(r *http.Request) string {
	var asked []language.Tag

	cookie, err := r.Cookie(langCookie)
	if err == nil && cookie.Value != "" {
		chosen := s.cookieTag(cookie.Value)
		if chosen == pseudoTag {
			return pseudoTag
		}

		tag, parseErr := language.Parse(chosen)
		if chosen != "" && parseErr == nil {
			return tag.String()
		}
	}

	if header := r.Header.Get("Accept-Language"); header != "" {
		if s.pseudo && strings.EqualFold(strings.TrimSpace(strings.Split(header, ",")[0]), pseudoTag) {
			return pseudoTag
		}

		tags, _, _ := language.ParseAcceptLanguage(header)
		asked = append(asked, tags...)
	}

	_, index, confidence := matcher.Match(asked...)
	if confidence == language.No {
		return englishTag
	}

	return supported[index].String()
}

// word is a message from a family keyed by a raw value (a status, a verb). The raw value still names the CSS class beside it; only the text is translated. A value with no entry shows as itself.
func (l *locale) word(family string, raw any) string {
	value := fmt.Sprint(raw)
	key := family + "." + value
	if _, ok := l.msgs[key]; !ok {
		return value
	}

	return l.text(key)
}

// actorLabel is actor.Label in loc's words. internal/actor keeps its English: the terminal prints that one.
func (l *locale) actorLabel(a actor.Actor) string {
	switch a.Kind {
	case "os":
		return l.text("actor.os", a.ID)
	case "check":
		return l.text("actor.check", a.ID)
	case "webhook":
		return l.text("actor.webhook", a.ID)
	case "upstream":
		return l.text("actor.upstream")
	case "backlog":
		return l.text("actor.backlog")
	case "":
		return a.ID
	default:
		return l.text("actor.other", a.ID, a.Kind)
	}
}

// actorTag is the one place a who is drawn, so the sentences that name one can take it as an argument.
func (l *locale) actorTag(stored string) template.HTML {
	if stored == "" {
		return ""
	}

	who := actor.Parse(stored)

	class := "actor"
	if who.Automatic() || who.Unverified() {
		class += " quiet"
	}

	return template.HTML(`<span class="` + class + `" title="` + html.EscapeString(stored) + `">` + html.EscapeString(l.actorLabel(who)) + `</span>`) //nolint:gosec // every dynamic part escaped here
}

var pluralSuffix = map[cldr.Form]string{cldr.Zero: "zero", cldr.One: "one", cldr.Two: "two", cldr.Few: "few", cldr.Many: "many", cldr.Other: "other"}

// count is a message that counts something: the catalog holds one text per plural form the language has (key.one, key.other, ...), and CLDR's rules pick, never an n == 1 test. n is %[1]d in the text; further values follow.
func (l *locale) count(key string, n int, args ...any) string {
	tag := language.English

	parsed, err := language.Parse(l.tag)
	if err == nil && !l.pseudo {
		tag = parsed
	}

	form := pluralSuffix[cldr.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0)]
	if _, ok := l.msgs[key+"."+form]; !ok {
		form = "other"
	}

	return l.text(key+"."+form, append([]any{n}, args...)...)
}

var english = sync.OnceValue(func() *locale {
	loc, err := loadLocale(englishTag)
	if err != nil {
		panic(err)
	}

	return loc
})

// englishLocale is what the helpers that predate locales (and the tests that pin them) speak.
func englishLocale() *locale { return english() }

// langChoice is one row of the footer's picker; names are in their own language, so a reader who cannot read the current one can still find theirs.
type langChoice struct{ Tag, Name string }

var languageNames = map[string]string{"en": "English", "zh-CN": "简体中文", "ja": "日本語", "es": "Español", "pt-BR": "Português (Brasil)"}

func languageChoices() []langChoice {
	choices := make([]langChoice, 0, len(supported))

	for _, tag := range supported {
		choices = append(choices, langChoice{Tag: tag.String(), Name: languageNames[tag.String()]})
	}

	return choices
}

// languagePicked reports whether the visitor chose a language, as opposed to getting the browser's.
func (s *Server) languagePicked(r *http.Request) bool {
	cookie, err := r.Cookie(langCookie)
	if err != nil || cookie == nil {
		return false
	}

	return s.cookieTag(cookie.Value) != ""
}

// cookieTag is the cookie's value when it names a language this server speaks, "" otherwise.
func (s *Server) cookieTag(value string) string {
	if s.pseudo && value == pseudoTag {
		return pseudoTag
	}

	for _, tag := range supported {
		if tag.String() == value {
			return value
		}
	}

	return ""
}

// handleSetLanguage remembers a visitor's choice, or forgets it for an empty one so the browser's answer applies again. A GET, so the picker is plain links with no form for a read-only page to withhold; it sets only the reader's own cookie and holds nothing server-side.
func (s *Server) handleSetLanguage(c *echo.Context) error {
	if tag := s.cookieTag(c.FormValue("lang")); tag != "" {
		setCookie(c, langCookie, tag, "/", 365*24*60*60)
	} else {
		setCookie(c, langCookie, "", "/", -1)
	}

	return c.Redirect(http.StatusSeeOther, returnTo(c, "/")) //nolint:wrapcheck // echo's write error is returned verbatim by every handler here
}
