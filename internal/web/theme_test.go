package web

import (
	"strings"
	"testing"
)

// TestThemeIsChosenBeforeFirstPaintAndTheToggleSpeaksToIt: the theme a reader pinned is applied by a script that runs before the stylesheet loads, or every reload in light flashes dark; and the toggle reaches storage only through the function that script defines, so the two can never disagree about where the choice lives. This is the seam the light theme crosses — the head script and app.js are each fine alone and wrong together if they name different keys.
func TestThemeIsChosenBeforeFirstPaintAndTheToggleSpeaksToIt(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "alpha", "beta")

	for _, page := range []string{"/", "/p/alpha", "/docs/README.md"} {
		_, body := get(t, server, page)

		head := between(t, body, "<head>", "</head>")
		script, stylesheet := strings.Index(head, "window.stepsTheme"), strings.Index(head, `href="/static/app.css"`)

		if script < 0 || stylesheet < 0 || script > stylesheet {
			t.Errorf("%s does not define window.stepsTheme in a script ahead of the stylesheet:\n%s", page, head)
		}

		if strings.Count(head, "steps-theme") != 1 {
			t.Errorf("%s names the storage key other than exactly once in its head", page)
		}

		if toggle, main := strings.Index(body, `id="themebtn"`), strings.Index(body, "<main"); toggle < strings.Index(body, `id="statusline"`) || toggle > main {
			t.Errorf("%s draws the theme toggle outside the statusline", page)
		}

		if button := between(t, body, `id="themebtn"`, "</button>"); strings.Contains(button, "aria-label") {
			t.Errorf("%s labels the toggle with an attribute; #statusline is morphed on every poll and would reset it — the label is CSS on html[data-theme]:\n%s", page, button)
		}
	}
}

// TestThemeToggleDrawsEveryStateAndOwnsNoKey: app.js changes the theme only through the head script's window.stepsTheme, and app.css has a label for each of the three states the toggle cycles through.
func TestThemeToggleDrawsEveryStateAndOwnsNoKey(t *testing.T) {
	t.Parallel()

	js, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(js), "window.stepsTheme(") || strings.Contains(string(js), "steps-theme") {
		t.Error("app.js must change the theme through window.stepsTheme and never name its storage key")
	}

	css, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}

	for _, state := range []string{`:root:not([data-theme]) .themebtn`, `[data-theme="light"] .themebtn`, `[data-theme="dark"] .themebtn`} {
		if !strings.Contains(string(css), state) {
			t.Errorf("app.css draws no label for the toggle in state %s", state)
		}
	}
}
