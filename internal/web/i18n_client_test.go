package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestClientWordsComeFromTheServer: app.js and app.css hold no words of their own, so every string they draw rides on the page, in the visitor's language.
func TestClientWordsComeFromTheServer(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)
	server.pseudo = true

	_, english := getAs(t, server, "/p/demo", nil)
	_, pseudo := getAs(t, server, "/p/demo", http.Header{"Accept-Language": {pseudoTag}})

	for _, want := range []string{
		`data-t-copied="copied"`, `data-t-addresscopied="address copied"`, `data-t-signedout="signed out · sign in again"`,
		`data-t-ago="%[1]s ago"`, `data-t-running="running %[1]s"`, `data-t-expired="expired"`,
		`data-auto="auto"`, `data-light="light"`, `data-dark="dark"`,
	} {
		if !strings.Contains(english, want) {
			t.Errorf("English page lacks %s", want)
		}
	}

	for _, attr := range []string{"data-t-copied", "data-t-addresscopied", "data-t-signedout", "data-t-ago", "data-t-running", "data-t-expired", "data-auto", "data-light", "data-dark"} {
		_, rest, _ := strings.Cut(pseudo, attr+`="`)
		value, _, _ := strings.Cut(rest, `"`)

		if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "···]") {
			t.Errorf("pseudo page: %s = %q, not mangled", attr, value)
		}
	}

	if !strings.Contains(pseudo, `data-t-ago="[%[1]s `) {
		t.Errorf("the ago pattern lost its placeholder in the pseudo-locale")
	}
}

// TestNoWordsInTheStaticFiles: a literal English word in app.js or app.css would stay English in every locale.
func TestNoWordsInTheStaticFiles(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	for _, banned := range []string{"= 'copied'", "'address copied'", "'signed out", "' ago'", "'running '", "? 'expired'"} {
		if strings.Contains(string(js), banned) {
			t.Errorf("app.js still spells %s itself", banned)
		}
	}

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	for _, banned := range []string{`content: "auto"`, `content: "light"`, `content: "dark"`} {
		if strings.Contains(string(css), banned) {
			t.Errorf("app.css still spells %s itself", banned)
		}
	}
}

// TestTheBrowserDrawsThePseudoWords: the ticker and the theme toggle are the two places script and stylesheet draw words, so they are read back from a real page in the pseudo-locale.
func TestTheBrowserDrawsThePseudoWords(t *testing.T) {
	ctx := browser(t)

	server, pipeline := testPipeline(t)
	server.pseudo = true

	seedPseudoFixtures(t, pipeline)

	listener := httptest.NewServer(server.Handler())
	t.Cleanup(listener.Close)

	str := chromedp.Evaluate[string]

	themeWord := must(ctx, t, str(`getComputedStyle(document.querySelector('.themelabel'), '::after').content`),
		chromedp.Navigate(listener.URL+"/p/demo/approvals"),
		chromedp.Evaluate[chromedp.Void](`document.cookie = 'steps-lang=en-XA; path=/'`),
		chromedp.Reload(),
		chromedp.WaitVisible(chromedp.CSS("time[data-ago]")))
	ago := must(ctx, t, str(`document.querySelector('time[data-ago]').textContent`))

	if !strings.Contains(themeWord, "[") || !strings.Contains(themeWord, "···]") {
		t.Errorf("the theme label drew %q, not the pseudo word", themeWord)
	}

	if !strings.HasPrefix(ago, "[") || !strings.HasSuffix(ago, "···]") {
		t.Errorf("the ticker drew %q, not the pseudo pattern", ago)
	}
}
