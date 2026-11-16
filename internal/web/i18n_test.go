package web

import (
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

func getAs(t *testing.T, server *Server, target string, header http.Header) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	for k, v := range header {
		req.Header[k] = v
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

// TestThePseudoLocaleReachesThePage is the seam: a request's Accept-Language picks a locale, that locale picks a template set, and the set's funcs say the locale's words. English stays the default and the pseudo-locale is never reachable unless a test turned it on.
func TestThePseudoLocaleReachesThePage(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	pseudo := http.Header{"Accept-Language": {"en-XA"}}

	_, plain := getAs(t, server, "/p/demo", pseudo)
	if !strings.Contains(plain, `<html lang="en">`) || strings.Contains(plain, "[") && strings.Contains(plain, "···]") {
		t.Fatal("the pseudo-locale answered without being enabled")
	}

	server.pseudo = true

	_, body := getAs(t, server, "/p/demo", pseudo)
	if !strings.Contains(body, `<html lang="en-XA">`) {
		t.Error("html lang does not follow the locale")
	}

	if !strings.Contains(body, "[") || !strings.Contains(body, "···]") {
		t.Error("no label came back mangled")
	}

	_, english := getAs(t, server, "/p/demo", nil)
	if !strings.Contains(english, `<html lang="en">`) {
		t.Error("English is no longer the default")
	}
}

func TestACookieBeatsTheHeader(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	server.pseudo = true

	_, body := getAs(t, server, "/p/demo", http.Header{
		"Accept-Language": {"fr"},
		"Cookie":          {"steps-lang=en-XA"},
	})
	if !strings.Contains(body, `<html lang="en-XA">`) {
		t.Error("the cookie did not win")
	}

	_, body = getAs(t, server, "/p/demo", http.Header{"Accept-Language": {"fr, de;q=0.5"}})
	if !strings.Contains(body, `<html lang="en">`) {
		t.Error("an unsupported language did not fall back to English")
	}
}

// TestCatalogTextIsEscapedAndOnlyMarkupArgumentsAreRaw: a translator's text and every plain argument go through the escaper; markup arrives only as an argument the caller marked safe.
func TestCatalogTextIsEscapedAndOnlyMarkupArgumentsAreRaw(t *testing.T) {
	t.Parallel()

	loc := &locale{tag: "en", msgs: map[string]string{
		"x.plain": "a <b> %[1]s",
		"x.link":  "see %[2]s for %[1]s",
	}}

	if got := loc.text("x.plain", "<script>"); got != "a <b> <script>" {
		t.Errorf("text() = %q: plain text is not escaped by this layer, html/template does it", got)
	}

	got := string(loc.html("x.link", "<img onerror=x>", template.HTML("<a href=\"/\">here</a>")))
	if want := `see <a href="/">here</a> for &lt;img onerror=x&gt;`; got != want {
		t.Errorf("html() = %q, want %q", got, want)
	}

	if got := string(loc.html("x.plain")); strings.Contains(got, "<b>") {
		t.Errorf("catalog markup was emitted raw: %q", got)
	}
}

var (
	dropBlocks = regexp.MustCompile(`(?is)<script\b.*?</script>|<style\b.*?</style>|<pre\b.*?</pre>|<code\b.*?</code>|<kbd\b.*?</kbd>|<svg\b.*?</svg>|<!--.*?-->|<h1 class="prompt">.*?</h1>|<nav class="langs".*?</nav>`)
	tagRe      = regexp.MustCompile(`(?s)<[^>]*>`)
	attrRe     = regexp.MustCompile(`\b(?:aria-label|title|placeholder|alt)="([^"]*)"`)
	wordRe     = regexp.MustCompile(`[A-Za-z]{2,}`)
)

// fixtureWords are data the pages echo back, never chrome: names the fixtures gave, the brand, and the build's version.
var fixtureWords = map[string]bool{
	"demo": true, "build": true, "deploy": true, "repo": true, "compile": true, "ship": true, "mock": true,
	"steps": true, "web": true, "dev": true, "JT": true, "Archie": true, "http": true, "https": true, "github": true, "com": true,
	"tmp": true, "ws": true, "yml": true, "ms": true, "beef": true, "fx": true, "task": true, "users": true, "jtarchie": true,
}

// dropEnglish removes elements marked lang="en": server messages and the docs body stay English on purpose, and the mark is what tells a screen reader so.
func dropEnglish(body string) string {
	for {
		at := strings.Index(body, ` lang="en"`)
		if at < 0 {
			return body
		}

		open := strings.LastIndex(body[:at], "<")
		name := strings.FieldsFunc(body[open+1:], func(r rune) bool { return r == ' ' || r == '>' })[0]

		end := strings.Index(body[at:], "</"+name+">")
		if end < 0 {
			return body[:open]
		}

		body = body[:open] + body[at+end+len(name)+3:]
	}
}

// untranslated is every visible word, and every aria-label, title or placeholder word, still in plain ASCII.
func untranslated(page, fixture string) []string {
	var left []string

	scan := func(text string) {
		for _, word := range wordRe.FindAllString(html.UnescapeString(text), -1) {
			if !fixtureWords[word] && !fixtureWords[strings.ToLower(word)] && !strings.Contains(fixture, word) {
				left = append(left, word)
			}
		}
	}

	_, body, found := strings.Cut(page, "<body")
	if found {
		body = "<body" + body
	}

	body = dropEnglish(dropBlocks.ReplaceAllString(body, ""))

	for _, match := range attrRe.FindAllStringSubmatch(body, -1) {
		scan(match[1])
	}

	scan(tagRe.ReplaceAllString(body, " "))

	return left
}

// TestEveryPageSpeaksOnlyThroughTheCatalog is the to-do list for #266: a word still in plain ASCII on a pseudo-locale page was written straight into a template or helper.
func TestEveryPageSpeaksOnlyThroughTheCatalog(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	server.pseudo = true

	seedPseudoFixtures(t, pipeline)

	for _, path := range []string{
		"/p/demo", "/p/demo/runs", "/p/demo/resources", "/p/demo/approvals", "/p/demo/questions",
		"/p/demo/jobs/build/detail", "/p/demo/resources/repo", "/p/demo/runs/fx1", "/", "/nosuch",
	} {
		_, page := getAs(t, server, path, http.Header{"Accept-Language": {pseudoTag}})

		if left := untranslated(page, t.Name()); len(left) > 0 {
			t.Errorf("%s: plain English outside the catalog: %v", path, slices.Compact(slices.Sorted(slices.Values(left))))
		}
	}
}

func seedPseudoFixtures(t *testing.T, pipeline *Pipeline) {
	t.Helper()

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "fx1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "fx1", []store.RunEventRow{
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", StepKind: "task", Text: "built\n"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", Status: "succeeded", Hash: "beef7654321", DurationMS: 42},
	})

	_, err = pipeline.Store.RequestApproval(t.Context(), "deploy", "ship")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
}
