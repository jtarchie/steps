package cli

import (
	"testing"

	"github.com/jtarchie/steps/docs"
)

// Piped, a page is its raw markdown, so `steps docs agents | grep` searches the page that was named and a bare `steps docs` the index; not parallel, it captures stdout.
func TestDocsPrintsThePageItWasAskedFor(t *testing.T) {
	for page, file := range map[string]string{"": "README.md", "agents": "agents.md", "resources.md": "resources.md"} {
		want, err := docs.Page(file)
		if err != nil {
			t.Fatalf("docs.Page(%s): %v", file, err)
		}

		out := captureStdout(t, func() { err = (&DocsCmd{Page: page}).Run() })
		if err != nil {
			t.Errorf("steps docs %q: %v", page, err)
		}

		if out != string(want) {
			t.Errorf("steps docs %q printed %d bytes, want %s's %d", page, len(out), file, len(want))
		}
	}
}
