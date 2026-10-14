package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/docs"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
)

// The docs and examples are what an author copies, so one that validate would
// warn about teaches the habit the warning exists to break. A built-in
// resource type is registered into every pipeline, so one unquoted template
// there would warn on every validate anybody runs; any loaded file covers them.
//
// A file needing vars or includes the e2e harnesses supply does not load here;
// those harnesses run validate on it, which prints the same warnings.
func TestDocsExamplesQuoteWhatReachesTheShell(t *testing.T) {
	t.Parallel()

	examples, err := filepath.Glob("../../examples/*.yml")
	if err != nil {
		t.Fatal(err)
	}

	loaded := 0
	for _, example := range examples {
		loaded += checkQuoted(t, example, example)
	}

	blocks, err := docs.Blocks()
	if err != nil {
		t.Fatal(err)
	}

	for _, block := range blocks {
		if block.Mode() == "fragment" {
			continue
		}

		path := filepath.Join(t.TempDir(), "pipeline.yml")

		err := os.WriteFile(path, []byte(block.Body), 0o600)
		if err != nil {
			t.Fatal(err)
		}

		loaded += checkQuoted(t, block.Name(), path)
	}

	if loaded == 0 {
		t.Fatal("nothing loaded, so nothing was checked")
	}
}

// checkQuoted reports path's warnings under name, returning 1 when it loaded.
func checkQuoted(t *testing.T, name, path string) int {
	t.Helper()

	cfg, err := config.LoadConfig(path)
	if err != nil {
		return 0
	}

	for _, warning := range pipeline.UnquotedShellValues(cfg) {
		t.Errorf("%s: %s", name, warning)
	}

	return 1
}
