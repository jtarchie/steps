package runview

import (
	"fmt"
	"strings"
	"testing"
)

// FuzzSlug: a step's URL fragment is lowercase alphanumeric runs joined by single dashes, and slugging a slug changes nothing.
func FuzzSlug(f *testing.F) {
	f.Add("review[security]")
	f.Add("--Build  Go 1.27!!")
	f.Add("ÉCOLE_ß")
	f.Add("")

	f.Fuzz(func(t *testing.T, name string) {
		slug := Slug(name)

		for part := range strings.SplitSeq(slug, "-") {
			if slug == "" {
				break
			}

			if part == "" || strings.Trim(part, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
				t.Fatalf("Slug(%q) = %q, not dash-joined [a-z0-9] runs", name, slug)
			}
		}

		if again := Slug(slug); again != slug {
			t.Fatalf("Slug(%q) = %q, but Slug(%q) = %q", name, slug, slug, again)
		}
	})
}

// FuzzParseDepth reads back what agent.transcript writes, and treats any other status as top level.
func FuzzParseDepth(f *testing.F) {
	f.Add(3, "ok")
	f.Add(-1, "depth:x")
	f.Add(0, "depth:12abc")

	f.Fuzz(func(t *testing.T, depth int, status string) {
		if got := parseDepth(fmt.Sprintf("depth:%d", depth)); got != depth {
			t.Fatalf("depth %d read back as %d", depth, got)
		}

		if !strings.HasPrefix(status, "depth:") && parseDepth(status) != 0 {
			t.Fatalf("status %q read as depth %d", status, parseDepth(status))
		}
	})
}
