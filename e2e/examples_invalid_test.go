package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// expectMarker introduces the error substring an invalid example must produce.
const expectMarker = "# expect:"

// TestExamplesInvalid is TestExamplesRun's mirror image: every file under
// examples/invalid/ must FAIL to load, with the error its `# expect:` line
// names.
//
// It exists because a rejection had nowhere to be demonstrated. examples/*.yml
// is globbed by four tests (here, schema_test.go, validate_test.go, and
// internal/config/strictyaml_test.go) that each require a file to load — so a
// pipeline whose entire purpose is to be rejected would pin the suite red
// forever. Every one of steps's load-time rejections therefore existed only as
// an error-substring assertion inside a Go test, with no file a human could
// read or docs could link to.
//
// All four of those globs are `examples/*.yml`, which does not descend into
// subdirectories — that non-recursion is what keeps this directory out of them,
// so a glob changed to `examples/**` would break the suite loudly rather than
// silently including deliberately-broken files.
//
// The `# expect:` substring is the load-bearing part. Asserting only "it
// failed" would pass on a typo in the pipeline: the file would still fail to
// load, just for a reason that has nothing to do with the rule under test.
func TestExamplesInvalid(t *testing.T) {
	matches, err := filepath.Glob(repoFile("examples", "invalid", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	if len(matches) == 0 {
		t.Fatal("no invalid examples found")
	}

	for _, path := range matches {
		t.Run(filepath.Base(path), func(t *testing.T) {
			want := expectedError(t, path)

			cfg, loadErr := config.LoadConfig(path)
			if loadErr == nil {
				t.Fatalf("%s loaded successfully, but examples/invalid/ files must be rejected "+
					"(expected an error containing %q)", path, want)
			}

			if cfg != nil {
				t.Errorf("%s: LoadConfig returned a config alongside its error", path)
			}

			if !strings.Contains(loadErr.Error(), want) {
				t.Errorf("%s failed for the wrong reason\n  want substring: %s\n  got error:      %v",
					path, want, loadErr)
			}
		})
	}
}

// expectedError pulls the `# expect: <substring>` line out of an invalid
// example. A file without one is a test failure, not a skip — otherwise the
// cheapest way to "add" a rejection example would be to assert nothing about
// it.
func expectedError(t *testing.T, path string) string {
	t.Helper()

	body, err := os.ReadFile(path) //nolint:gosec // path comes from this test's own glob over the repo
	if err != nil {
		t.Fatal(err)
	}

	for line := range strings.Lines(string(body)) {
		_, after, found := strings.Cut(line, expectMarker)
		if !found {
			continue
		}

		want := strings.TrimSpace(after)
		if want == "" {
			t.Fatalf("%s has an empty %q line; name the error substring it must produce", path, expectMarker)
		}

		return want
	}

	t.Fatalf("%s has no %q line; every invalid example must name the error it produces", path, expectMarker)

	return ""
}
