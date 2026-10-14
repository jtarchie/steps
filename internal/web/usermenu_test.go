package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheUserMenuNamesWhoIsSignedIn: last on the line, after jump, and only where somebody signs in.
func TestTheUserMenuNamesWhoIsSignedIn(t *testing.T) {
	t.Parallel()

	_, pipeline := testPipeline(t)

	server, err := New([]*Pipeline{pipeline}, nil, WithBasicAuth("ops", "pw"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/p/demo", nil)
	req.SetBasicAuth("ops", "pw")

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	jump, menu := strings.Index(body, `id="jumpbtn"`), strings.Index(body, `id="userbtn"`)
	if jump < 0 || menu < jump {
		t.Fatalf("the user menu is missing or comes before jump (jump %d, menu %d)", jump, menu)
	}

	for _, want := range []string{`<span class="who">ops</span>`, `aria-haspopup="menu"`, `<a role="menuitem" href="/activity">activity</a>`} {
		if !strings.Contains(body, want) {
			t.Errorf("the user menu does not carry %s", want)
		}
	}

	open, _ := testPipeline(t)

	_, anonymous := get(t, open, "/p/demo")
	if strings.Contains(anonymous, `id="userbtn"`) {
		t.Error("a server nobody signs in to drew a user menu")
	}
}

// TestTheMenusShareOneOpenStateAndEscape: delegated like the switcher was, so a poll's morph cannot strand a listener, and Escape returns focus to the button that opened it. No JS runtime runs here; Firefox against a daemon is the behavioural check.
func TestTheMenusShareOneOpenStateAndEscape(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{`pipebtn: 'pipemenu', userbtn: 'usermenu'`, `closest('#pipebtn, #userbtn')`, `e.key !== 'Escape'`, `btn.focus()`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js does not carry %s", want)
		}
	}
}
