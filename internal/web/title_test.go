package web

import (
	"strings"
	"testing"
)

// TestEveryPageSaysWhereItIs: a page titled just "steps" is a browser tab and a history entry nobody can tell from the next one.
func TestEveryPageSaysWhereItIs(t *testing.T) {
	t.Parallel()

	server, _ := testPipelines(t, "app", "infra")

	for path, title := range map[string]string{
		"/":                "pipelines — steps",
		"/p/app":           "app — steps",
		"/p/app/runs":      "runs — steps",
		"/p/app/resources": "resources — steps",
		"/p/app/approvals": "approvals — steps",
		"/p/app/questions": "questions — steps",
	} {
		_, body := get(t, server, path)
		if !strings.Contains(body, "<title>"+title+"</title>") {
			_, rest, _ := strings.Cut(body, "<title>")
			got, _, _ := strings.Cut(rest, "</title>")
			t.Errorf("%s is titled %q, want %q", path, got, title)
		}
	}
}
