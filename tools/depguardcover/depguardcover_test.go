package depguardcover_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type rule struct {
	Files []string `yaml:"files"`
	Allow []string `yaml:"allow"`
}

func TestEveryInternalPackageHasADepguardRule(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../.golangci.yml")
	if err != nil {
		t.Fatal(err)
	}

	var config struct {
		Linters struct {
			Settings struct {
				Depguard struct {
					Rules map[string]rule `yaml:"rules"`
				} `yaml:"depguard"`
			} `yaml:"settings"`
		} `yaml:"linters"`
	}

	err = yaml.Unmarshal(raw, &config)
	if err != nil {
		t.Fatal(err)
	}

	rules := config.Linters.Settings.Depguard.Rules
	if len(rules) < 10 {
		t.Fatalf("read %d depguard rules; the config walk has stopped working, and no rules would pass for every package covered", len(rules))
	}

	root, err := filepath.Abs("../../internal")
	if err != nil {
		t.Fatal(err)
	}

	packages := packageFiles(t, root)
	if len(packages) < 10 {
		t.Fatalf("found %d packages under %s; the walk has stopped working, and none would pass for all covered", len(packages), root)
	}

	for dir, file := range packages {
		if !allowListed(rules, file) {
			t.Errorf("%s matches no depguard rule with an allow: list, so it may import anything; add one to .golangci.yml", dir)
		}
	}
}

// packageFiles is one .go file per package directory, a non-test one when there is any, since that is what an allow-list governs.
func packageFiles(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() && entry.Name() == "testdata" {
			return filepath.SkipDir
		}

		if entry.IsDir() || filepath.Ext(path) != ".go" {
			return nil
		}

		dir := filepath.Dir(path)
		if existing, ok := out[dir]; !ok || (strings.HasSuffix(existing, "_test.go") && !strings.HasSuffix(path, "_test.go")) {
			out[dir] = path
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// allowListed reports whether some rule with an allow: list applies to file, by depguard's reading of files: — any positive pattern matches and no negated one does.
func allowListed(rules map[string]rule, file string) bool {
	for _, r := range rules {
		if len(r.Allow) == 0 {
			continue
		}

		matched, excluded := false, false

		for _, pattern := range r.Files {
			negated := strings.HasPrefix(pattern, "!")
			if globMatches(strings.TrimPrefix(pattern, "!"), file) {
				if negated {
					excluded = true
				} else {
					matched = true
				}
			}
		}

		if matched && !excluded {
			return true
		}
	}

	return false
}

func globMatches(pattern, file string) bool {
	switch pattern {
	case "$all":
		return true
	case "$test":
		return strings.HasSuffix(file, "_test.go")
	}

	expr := regexp.QuoteMeta(pattern)
	expr = strings.ReplaceAll(expr, `\*\*`, "\x00")
	expr = strings.ReplaceAll(expr, `\*`, "[^/]*")
	expr = strings.ReplaceAll(expr, "\x00", ".*")

	return regexp.MustCompile("^" + expr + "$").MatchString(filepath.ToSlash(file))
}
