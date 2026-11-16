package web

import (
	cldr "golang.org/x/text/feature/plural"
	"golang.org/x/text/language"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// dynamicFamilies are keys built at runtime ("status." + a stored value), which no literal names; each prefix is a family whose members are chosen by data.
var dynamicFamilies = []string{"status.", "cell.", "verb.", "filter.", "mcp.status.", "follow.check.", "connect.", "change.", "time.ago.", "time.in.", "actor.", "graph.edge."}

var (
	verbRe     = regexp.MustCompile(`%\[(\d+)\][a-zA-Z]`)
	pluralRe   = regexp.MustCompile(`\.(zero|one|two|few|many|other)$`)
	templateKe = regexp.MustCompile(`\b(?:t|th|tn)\s+"([^"]+)"`)
	goKey      = regexp.MustCompile(`\b(?:text|html|count|say)\(\s*(?:c,\s*)?"([a-z][a-z0-9.]*[a-z0-9])"`)
)

func sources(t *testing.T) (templates, code string) {
	t.Helper()

	var tpl, goSrc strings.Builder

	files, _ := filepath.Glob("templates/*.html")
	for _, f := range files {
		raw, err := os.ReadFile(f) //nolint:gosec // a glob of this package's own files
		if err != nil {
			t.Fatal(err)
		}

		tpl.Write(raw)
	}

	goFiles, _ := filepath.Glob("*.go")
	for _, f := range goFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}

		raw, err := os.ReadFile(f) //nolint:gosec // a glob of this package's own files
		if err != nil {
			t.Fatal(err)
		}

		goSrc.Write(raw)
	}

	return tpl.String(), goSrc.String()
}

func verbsOf(text string) []string {
	found := verbRe.FindAllStringSubmatch(text, -1)
	verbs := make([]string, 0, len(found))

	for _, match := range found {
		verbs = append(verbs, match[0])
	}

	slices.Sort(verbs)

	return slices.Compact(verbs)
}

func isDynamic(key string) bool {
	return slices.ContainsFunc(dynamicFamilies, func(prefix string) bool { return strings.HasPrefix(key, prefix) })
}

// TestEveryKeyIsUsed: a key nothing asks for is dead text a translator would still be paid to translate.
func TestEveryKeyIsUsed(t *testing.T) {
	t.Parallel()

	english, err := loadLocale(englishTag)
	if err != nil {
		t.Fatal(err)
	}

	tpl, code := sources(t)
	all := tpl + "\n" + code

	for key := range english.msgs {
		base := pluralRe.ReplaceAllString(key, "")
		if strings.Contains(all, `"`+key+`"`) || strings.Contains(all, `"`+base+`"`) || isDynamic(key) {
			continue
		}

		t.Errorf("catalog key %q is not used by any template or helper", key)
	}
}

// TestEveryUseHasAKey: a literal key with no entry renders as itself, in every language.
func TestEveryUseHasAKey(t *testing.T) {
	t.Parallel()

	english, err := loadLocale(englishTag)
	if err != nil {
		t.Fatal(err)
	}

	tpl, code := sources(t)
	used := map[string]bool{}

	for _, match := range templateKe.FindAllStringSubmatch(tpl, -1) {
		used[match[1]] = true
	}

	for _, match := range goKey.FindAllStringSubmatch(code, -1) {
		used[match[1]] = true
	}

	for key := range used {
		_, plain := english.msgs[key]
		_, counted := english.msgs[key+".other"]

		if !plain && !counted && !strings.HasSuffix(key, ".") && !isDynamic(key) {
			t.Errorf("%q is asked for but the English catalog has no such key", key)
		}
	}
}

// TestEveryTranslationKeepsItsValues: a translator may reorder %[n] verbs but not drop or invent one.
func TestEveryTranslationKeepsItsValues(t *testing.T) {
	t.Parallel()

	english, err := loadLocale(englishTag)
	if err != nil {
		t.Fatal(err)
	}

	for _, tag := range supported {
		loc, loadErr := loadLocale(tag.String())
		if loadErr != nil {
			t.Fatal(loadErr)
		}

		for key, want := range english.msgs {
			text, ok := loc.msgs[key]
			if !ok {
				if !hasForm(tag, key) {
					continue
				}

				t.Errorf("%s: missing %q", tag, key)

				continue
			}

			if !slices.Equal(verbsOf(want), verbsOf(text)) {
				t.Errorf("%s: %q takes %v, English takes %v", tag, key, verbsOf(text), verbsOf(want))
			}
		}

		for key := range loc.msgs {
			if _, ok := english.msgs[key]; !ok {
				t.Errorf("%s: %q is not in the English catalog", tag, key)
			}
		}
	}
}

// TestEveryPluralFormSaysItsCount: a form that never says its count reads as a different sentence for 1 than for 5.
func TestEveryPluralFormSaysItsCount(t *testing.T) {
	t.Parallel()

	english, err := loadLocale(englishTag)
	if err != nil {
		t.Fatal(err)
	}

	for key, text := range english.msgs {
		if pluralRe.MatchString(key) && !strings.Contains(text, "%[1]") {
			t.Errorf("plural form %q never says its count", key)
		}
	}
}

// hasForm is false for a plural form the language does not have (Chinese has only "other"), which its catalog rightly leaves out; every other key is required.
func hasForm(tag language.Tag, key string) bool {
	match := pluralRe.FindStringSubmatch(key)
	if match == nil {
		return true
	}

	for n := range 200 {
		if pluralSuffix[cldr.Cardinal.MatchPlural(tag, n, 0, 0, 0, 0)] == match[1] {
			return true
		}
	}

	return false
}
