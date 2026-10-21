package web

import "testing"

// Agent prose is untrusted, and a model-written link that browsers read as another host is a way off the site under this site's own styling.
func TestProseLinksNeverLeaveTheSiteByAPathSpelling(t *testing.T) {
	t.Parallel()

	for destination, want := range map[string]string{
		"/p/demo/runs/1":    "/p/demo/runs/1",
		"#top":              "#top",
		"//evil.example":    "",
		"/\\evil.example":   "",
		"/\t/evil.example":  "",
		"/\n/evil.example":  "",
		"https://ok.test/a": "https://ok.test/a",
	} {
		if got := safeURL(destination); got != want {
			t.Errorf("safeURL(%q) = %q, want %q", destination, got, want)
		}
	}
}
