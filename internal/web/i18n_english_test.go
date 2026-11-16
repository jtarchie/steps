package web

import (
	"slices"
	"testing"
)

// sameAsEnglish lists the entries a translation may legitimately leave word-for-word English (loanwords, product terms, cognates); anything else identical to English is an untranslated string that slipped through.
var sameAsEnglish = map[string][]string{
	"tab.mcp": {"ja", "zh-CN", "es", "pt-BR"}, "tool.mcp": {"ja", "zh-CN", "es", "pt-BR"}, "dials.tokens": {"ja", "zh-CN", "es", "pt-BR"}, "step.tokens": {"ja", "zh-CN", "es", "pt-BR"},
	"actor.webhook":   {"ja", "es", "pt-BR"},
	"filter.pipeline": {"es", "pt-BR"}, "nav.pipeline": {"es", "pt-BR"}, "nav.pipelines": {"es", "pt-BR"}, "nav.pipelines.label": {"es", "pt-BR"},
	"overview.count": {"es", "pt-BR"}, "overview.keys.pipelines": {"es", "pt-BR"}, "th.pipeline": {"es", "pt-BR"},
	"node.error": {"es"}, "overview.manual": {"es", "pt-BR"}, "json.array": {"es", "pt-BR"}, "json.items.one": {"pt-BR"}, "th.status": {"pt-BR"},
}

// TestNoTranslationIsLeftInEnglish: a catalog entry copied verbatim from English is the same bug as a missing one and nothing else notices it, so it must be named in sameAsEnglish on purpose.
func TestNoTranslationIsLeftInEnglish(t *testing.T) {
	t.Parallel()

	english, err := loadLocale(englishTag)
	if err != nil {
		t.Fatal(err)
	}

	for _, tag := range supported {
		if tag.String() == englishTag {
			continue
		}

		loc, loadErr := loadLocale(tag.String())
		if loadErr != nil {
			t.Fatal(loadErr)
		}

		for key, want := range english.msgs {
			got, ok := loc.msgs[key]
			if !ok || got != want || !wordRe.MatchString(verbRe.ReplaceAllString(got, "")) {
				continue
			}

			if !slices.Contains(sameAsEnglish[key], tag.String()) {
				t.Errorf("%s: %q is still English (%q); translate it, or list it in sameAsEnglish with a reason", tag, key, got)
			}
		}
	}
}
