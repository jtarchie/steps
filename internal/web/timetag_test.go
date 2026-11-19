package web

import (
	"strings"
	"testing"
	"time"
)

// TestTimeTagsEscapeTheirWords: the relative-time words come from a translator's catalog, and these tags are built by hand rather than by html/template, so a catalog entry carrying markup or a quote must arrive as text.
func TestTimeTagsEscapeTheirWords(t *testing.T) {
	t.Parallel()

	hostile := `<b onclick="x">`
	loc := &locale{tag: "en", msgs: map[string]string{
		"time.now.short": hostile, "time.now.spoken": hostile, "time.justnow": hostile,
	}}

	for name, tag := range map[string]string{
		"nextTag":    string(nextTagIn(loc, time.Now())),
		"agoCompact": string(agoCompactTagIn(loc, time.Now())),
	} {
		if strings.Contains(tag, "<b") || !strings.Contains(tag, "&lt;b onclick=&#34;x&#34;&gt;") {
			t.Errorf("%s did not escape its words: %s", name, tag)
		}
	}
}
