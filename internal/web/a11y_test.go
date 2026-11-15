package web

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// TestEveryPageOffersASkipToItsContent: the status bar and the action bar sit before every page's content, so a keyboard reader tabbed through a dozen controls to reach it (WCAG 2.4.1). The skip link must be the FIRST thing focusable, its target must take focus, and the bar must be a <header> landmark so a screen reader's landmark list names it rather than leaving it loose outside every region.
func TestEveryPageOffersASkipToItsContent(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	for _, path := range []string{"/p/demo", "/p/demo/runs", "/p/demo/resources", "/p/demo/approvals", "/p/demo/questions", "/docs/README.md", "/nosuch"} {
		_, page := get(t, server, path)

		_, body, _ := strings.Cut(page, "<body")
		first := regexp.MustCompile(`<(a|button|input|select|textarea)\b[^>]*>`).FindString(body)

		if !strings.Contains(first, `href="#main"`) {
			t.Errorf("%s: the first focusable element is %s, not a skip to #main", path, first)
		}

		if !strings.Contains(page, `<main id="main" tabindex="-1">`) {
			t.Errorf("%s: <main> is not a skip target that can take focus", path)
		}

		header, bar, headerEnd, main := strings.Index(page, "<header"), strings.Index(page, `id="statusline"`), strings.Index(page, "</header>"), strings.Index(page, "<main")
		if header < 0 || header > bar || headerEnd < bar || headerEnd > main {
			t.Errorf("%s: the status bar is not inside a <header> landmark before <main>", path)
		}
	}
}

// TestDocsTablesNameEveryColumn: an empty header cell is a column a screen reader announces as nothing while reading each row.
func TestDocsTablesNameEveryColumn(t *testing.T) {
	t.Parallel()

	for _, name := range docs.Pages() {
		body, err := docs.Page(name)
		if err != nil {
			t.Fatal(err)
		}

		out, err := renderDocsMarkdown(body)
		if err != nil {
			t.Fatal(err)
		}

		if strings.Contains(out, "<th></th>") {
			t.Errorf("%s: a table has an empty header cell", name)
		}
	}
}

// TestAStepHeadIsNotAButtonAroundLinks: the head used to be role="button" holding the permalink and the node link — a control inside a control, which a screen reader flattens to one button and a keyboard reaches by two routes (WCAG 4.1.2). The toggle is a real <button> beside them that says whether its row is open.
func TestAStepHeadIsNotAButtonAroundLinks(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)

	err := pipeline.Store.StartRun(t.Context(), store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	appendEvents(t, pipeline.Store, "run-1", []store.RunEventRow{
		{Type: events.TypeJobStarted, StepIndex: -1},
		{Type: events.TypeStepStarted, StepIndex: 0, StepName: "compile", StepKind: "task"},
		{Type: events.TypeStepOutput, StepIndex: 0, StepName: "compile", StepKind: "task", Text: "built\n"},
		{Type: events.TypeStepFinished, StepIndex: 0, StepName: "compile", StepKind: "task", Status: "succeeded", Hash: "beef7654321", DurationMS: 42},
	})

	_, page := get(t, server, "/p/demo/runs/run-1")

	heads := regexp.MustCompile(`(?s)<div class="stephead" id="([^"]+)_head"[^>]*>.*?</div>`).FindAllStringSubmatch(page, -1)
	if len(heads) == 0 {
		t.Fatal("the run page drew no step heads; this test would be checking nothing")
	}

	toggles := 0

	for _, head := range heads {
		if tag, _, _ := strings.Cut(head[0], ">"); strings.Contains(tag, "role=") || strings.Contains(tag, "tabindex") {
			t.Errorf("%s: the head itself is still a control:\n%s", head[1], head[0])
		}

		button := regexp.MustCompile(`<button type="button" class="steptoggle" aria-expanded="(?:true|false)" aria-controls="([^"]+)">(?s:.*?)</button>`).FindStringSubmatch(head[0])
		if button == nil {
			continue
		}

		toggles++

		checkToggle(t, page, head[1], button)
	}

	if toggles == 0 {
		t.Errorf("no step head carries a toggle button:\n%s", heads[0][0])
	}
}

// TestTheMenusAreDisclosuresNotListboxes: the pipeline and user menus are a button that shows a list of links. role="listbox"/"menu" promised a screen reader arrow keys and typeahead the script never implemented, so a reader in one heard "option 1 of 3" and could not move; links under an aria-expanded button are what they are.
func TestTheMenusAreDisclosuresNotListboxes(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, page := get(t, server, "/p/demo")
	header, _, _ := strings.Cut(page, "</header>")

	for _, role := range []string{`role="listbox"`, `role="option"`, `role="menu"`, `role="menuitem"`, `aria-haspopup`, `aria-selected`} {
		if strings.Contains(header, role) {
			t.Errorf("the header still carries %s", role)
		}
	}

	if !strings.Contains(page, `aria-expanded="false" aria-controls="pipemenu"`) {
		t.Error("the switcher button does not name the list it opens")
	}

	if !strings.Contains(page, `aria-current="page" href="/p/demo"`) {
		t.Error("the switcher does not mark the pipeline being read")
	}
}

// TestSingleKeyShortcutsCanBeTurnedOff: j/k/e/c/f and / act on one keypress anywhere on the page, which a speech-input user fires by saying a word with that letter in it (WCAG 2.1.4). Every page that teaches the keys offers the switch beside them — outside the aria-hidden hint, since a control a screen reader is told is not there is one it cannot press.
func TestSingleKeyShortcutsCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	runServer, pipeline := testPipeline(t)
	startFinishedRun(t, pipeline, "run-0", "build", "succeeded")
	rootServer, _ := lanePipelines(t, nil, "app", twoJobs, "other", oneManualJob)

	for path, server := range map[string]*Server{"/p/demo/runs/run-0": runServer, "/": rootServer} {
		_, page := get(t, server, path)

		hint := regexp.MustCompile(`(?s)<div class="keyhint">.*?</div>`).FindString(page)
		if hint == "" {
			t.Fatalf("%s: no key hint", path)
		}

		if !strings.Contains(hint, `<button type="button" class="keystoggle">`) {
			t.Errorf("%s: the key hint offers no way to turn the keys off:\n%s", path, hint)
		}

		for _, hidden := range regexp.MustCompile(`(?s)<span class="keys" aria-hidden="true">.*?</span>\s*<button`).FindAllString(hint, -1) {
			if strings.Count(hidden, "<button") > 1 || strings.Contains(hidden, "<a ") {
				t.Errorf("%s: a control sits inside the aria-hidden hint:\n%s", path, hidden)
			}
		}
	}
}

func checkToggle(t *testing.T, page, row string, button []string) {
	t.Helper()

	if strings.Contains(button[0], "<a ") {
		t.Errorf("%s: a link is still inside the toggle:\n%s", row, button[0])
	}

	for _, id := range strings.Fields(button[1]) {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("%s: the toggle controls #%s, which the page does not draw", row, id)
		}
	}
}

// TestThePaletteIsACombobox: the jump palette is a text box whose arrow keys move through results the script draws — the one control here that IS a combobox. Without the roles a screen reader heard a bare edit field and nothing of the results or which one Enter would open.
func TestThePaletteIsACombobox(t *testing.T) {
	t.Parallel()

	server, _ := testPipeline(t)

	_, page := get(t, server, "/p/demo")

	for _, want := range []string{
		`<input id="palette-input" type="text" role="combobox" aria-label="Jump to" aria-expanded="false" aria-controls="palette-list" aria-autocomplete="list"`,
		`<ul id="palette-list" role="listbox" aria-label="Results">`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the palette lacks %s", want)
		}
	}
}
