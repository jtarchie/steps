package resource

import (
	"strings"
	"testing"
)

// TestCheckCloneURLKeepsTheTokenOffCleartext: the clone URL comes from GitHub's answer and the token goes with it, so it is held to source.endpoint's rule.
func TestCheckCloneURLKeepsTheTokenOffCleartext(t *testing.T) {
	t.Parallel()

	for raw, ok := range map[string]bool{
		"https://github.com/acme/app.git":    true,
		"http://127.0.0.1:8080/acme/app.git": true,
		"http://localhost/acme/app.git":      true,
		"http://github.com/acme/app.git":     false,
		"git@github.com:acme/app.git":        false,
		"ssh://git@github.com/acme/app.git":  false,
		"/srv/acme/app.git":                  false,
		"":                                   false,
	} {
		err := checkCloneURL(raw)
		if (err == nil) != ok {
			t.Errorf("checkCloneURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

// TestCheckoutGitRefusesACommitSpelledAsAnOption: a version's sha is pinned by hand too, and git would read --upload-pack=... in its argv as an option.
func TestCheckoutGitRefusesACommitSpelledAsAnOption(t *testing.T) {
	t.Parallel()

	for _, sha := range []string{"--upload-pack=touch /tmp/owned", "main", "ABCDEF0123456789abcdef0123456789abcdef01", ""} {
		err := checkoutGit(t.Context(), gitClone{url: "https://github.com/acme/app.git", sha: sha, number: "7", base: "main", dir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "is not a commit id") {
			t.Errorf("checkoutGit(sha %q) = %v, want it refused before git runs", sha, err)
		}
	}
}
